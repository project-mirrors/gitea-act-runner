// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const (
	jobLabel      = "com.gitea.runner.job"
	maxCreateBody = 8 << 20

	dockerProxyProbeTimeout = 5 * time.Second
)

var (
	createPath    = regexp.MustCompile(`^(/v[0-9.]+)?/(containers|networks|volumes)/create$`)
	rawStreamPath = regexp.MustCompile(`^(/v[0-9.]+)?/(containers/[^/]+/attach|exec/[^/]+/start)$`)
)

func NewDockerProxy(ctx context.Context, job string) *DockerProxy {
	if host := os.Getenv("DOCKER_HOST"); runtime.GOOS != "linux" || host != "" && !strings.HasPrefix(host, "unix://") {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, dockerProxyProbeTimeout)
	defer cancel()
	cli, err := GetDockerClient(probeCtx)
	if err != nil {
		return nil
	}
	defer cli.Close()
	daemonSocket, ok := strings.CutPrefix(cli.DaemonHost(), "unix://")
	if !ok {
		return nil
	}
	if info, err := os.Stat(daemonSocket); err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil
	}
	dir, daemonDir := runnerContainerWorkdir(probeCtx, cli)
	seen := daemonDir != ""
	if !seen {
		if dir, err = filepath.Abs(os.TempDir()); err == nil {
			daemonDir = dir
			seen, err = daemonSeesDir(probeCtx, cli, dir, daemonDir)
		}
	}
	if err != nil {
		common.Logger(ctx).Infof("docker proxy probe failed, jobs get the daemon socket directly: %v", err)
		return nil
	}
	if !seen {
		common.Logger(ctx).Infof("the docker daemon cannot reach the runner's temporary or working directory, jobs get the daemon socket directly")
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	proxy, err := StartDockerProxy(daemonSocket, dir, job)
	if err != nil {
		common.Logger(ctx).Warnf("docker proxy not started, the job gets the daemon socket directly: %v", err)
		return nil
	}
	proxy.Socket = daemonDir + strings.TrimPrefix(proxy.Socket, dir)
	return proxy
}

// runnerContainerWorkdir looks the runner's container up by hostname to find the daemon's path to its working directory.
func runnerContainerWorkdir(ctx context.Context, cli client.APIClient) (workdir, daemonDir string) {
	workdir, err := os.Getwd()
	hostname, hostnameErr := os.Hostname()
	if err != nil || hostnameErr != nil {
		return "", ""
	}
	self, err := cli.ContainerInspect(ctx, hostname, client.ContainerInspectOptions{})
	if err != nil {
		return "", ""
	}
	if daemonDir = containerInfoFromInspect(self.Container).DaemonPath(workdir); daemonDir == "" {
		return "", ""
	}
	if seen, _ := containerSeesMarker(ctx, cli, workdir, self.Container.ID, workdir); !seen { // the hostname may name another container
		return "", ""
	}
	return workdir, daemonDir
}

func containerSeesMarker(ctx context.Context, cli client.APIClient, dir, id, containerDir string) (bool, error) {
	marker, err := os.CreateTemp(dir, "gitea-runner-probe-")
	if err != nil {
		return false, err
	}
	defer func() {
		if err := os.Remove(marker.Name()); err != nil {
			common.Logger(ctx).Warnf("removing the docker proxy probe marker failed: %v", err)
		}
	}()
	if err := marker.Close(); err != nil {
		return false, err
	}
	_, err = cli.ContainerStatPath(ctx, id, client.ContainerStatPathOptions{Path: path.Join(containerDir, filepath.Base(marker.Name()))})
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// daemonSeesDir reports whether the daemon opens the files the runner writes in dir by their path in daemonDir,
// which is what a job's proxy socket mounted from there needs.
func daemonSeesDir(ctx context.Context, cli client.APIClient, dir, daemonDir string) (bool, error) {
	images, err := cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return false, err
	}
	if len(images.Items) == 0 {
		return false, errors.New("no image available for the docker proxy probe")
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: images.Items[0].ID, Cmd: []string{"true"}},
		HostConfig: &container.HostConfig{Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: daemonDir, Target: "/gitea-runner-probe", ReadOnly: true}, // not the marker itself, podman creates a missing bind source where docker rejects it
		}},
	})
	if cerrdefs.IsInvalidArgument(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	seen, err := containerSeesMarker(ctx, cli, dir, created.ID, "/gitea-runner-probe")
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerProxyProbeTimeout)
	defer cancel()
	if _, removeErr := cli.ContainerRemove(cleanupCtx, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); removeErr != nil {
		return false, fmt.Errorf("removing the docker proxy probe container failed: %w", removeErr)
	}
	return seen, err
}

// StartDockerProxy serves a job's docker socket in dir, labelling what the job creates through it.
func StartDockerProxy(daemonSocket, dir, job string) (*DockerProxy, error) {
	info, err := os.Stat(daemonSocket)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("docker daemon path is not a Unix socket")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	instance, err := os.MkdirTemp(dir, "p-")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(instance, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(instance))
	}
	if err := copyDockerSocketPermissions(daemonSocket, socket, info); err != nil {
		return nil, errors.Join(err, listener.Close(), os.RemoveAll(instance))
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", daemonSocket)
	}
	transport := &http.Transport{DialContext: dial}
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = "docker"
		},
		Transport: transport,
	}
	proxy := &DockerProxy{Socket: socket}
	streams, cancelStreams := context.WithCancel(context.Background())
	creates, cancelCreates := context.WithCancel(context.Background())
	var admission sync.Mutex
	var handlers sync.WaitGroup
	server := &http.Server{ReadHeaderTimeout: 30 * time.Second, ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, dockerProxyConnKey{}, conn)
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admission.Lock()
		if streams.Err() != nil {
			admission.Unlock()
			http.Error(w, "docker proxy is closing", http.StatusServiceUnavailable)
			return
		}
		handlers.Add(1)
		admission.Unlock()
		defer handlers.Done()
		creating := r.Method == http.MethodPost && createPath.MatchString(r.URL.Path)
		parent, lifetime := r.Context(), streams
		if creating {
			parent, lifetime = context.WithoutCancel(parent), creates
		}
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		stop := context.AfterFunc(lifetime, func() {
			cancel()
			if !creating {
				if conn, ok := parent.Value(dockerProxyConnKey{}).(net.Conn); ok {
					_ = conn.Close()
				}
			}
		})
		defer stop()
		r = r.WithContext(ctx)
		if creating {
			r.Body = http.MaxBytesReader(w, r.Body, maxCreateBody)
			mounts, _ := proxy.mounts.Load().(map[string]string)
			if err := rewriteCreate(r, job, mounts); err != nil {
				status := http.StatusBadRequest
				if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
					status = http.StatusRequestEntityTooLarge
				}
				http.Error(w, err.Error(), status)
				return
			}
		} else if r.Method == http.MethodPost && rawStreamPath.MatchString(r.URL.Path) {
			tunnel(w, r, dial, forward)
			return
		}
		forward.ServeHTTP(w, r)
	})}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	proxy.close = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		admission.Lock()
		listenerErr := listener.Close()
		cancelStreams()
		admission.Unlock()
		<-served
		shutdownErr := server.Shutdown(ctx)
		cancelCreates()
		serverErr := server.Close()
		handlers.Wait()
		transport.CloseIdleConnections()
		return errors.Join(ctx.Err(), listenerErr, shutdownErr, serverErr, os.RemoveAll(instance))
	}
	return proxy, nil
}

func rewriteCreate(r *http.Request, job string, mounts map[string]string) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	var fields map[string]json.RawMessage
	var config struct{ Labels map[string]string }
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("invalid create request: %w", err)
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return fmt.Errorf("invalid create labels: %w", err)
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	if len(mounts) > 0 && !hasAmbiguousFields(body) {
		translateBinds(fields, createPath.FindStringSubmatch(r.URL.Path)[2], mounts)
	}
	maps.DeleteFunc(fields, func(name string, _ json.RawMessage) bool {
		return strings.EqualFold(name, "Labels")
	})
	if config.Labels == nil {
		config.Labels = make(map[string]string)
	}
	config.Labels[jobLabel] = job
	if fields["Labels"], err = json.Marshal(config.Labels); err != nil {
		return err
	}
	if body, err = json.Marshal(fields); err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	return nil
}

func translateBinds(fields map[string]json.RawMessage, kind string, mounts map[string]string) {
	cleanedSource := func(source string) string {
		cleaned := path.Clean(source)
		if target := jobMount(cleaned, mounts); mounts[target] != "" {
			return mounts[target] + cleaned[len(target):]
		}
		return source
	}
	spelledSource := func(source string) string { // dockerd checks these as spelled
		target := jobMount(path.Clean(source), mounts)
		if rest, spelled := strings.CutPrefix(source, target); mounts[target] != "" && spelled && (rest == "" || rest[0] == '/') && filepath.IsLocal("."+rest) {
			return mounts[target] + rest
		}
		return cleanedSource(source)
	}
	switch kind {
	case "volumes":
		var driver string
		var options map[string]any
		decodeField(fields, "Driver", &driver)
		if key := decodeField(fields, "DriverOpts", &options); options != nil {
			translateDevice(driver, options, spelledSource)
			if encoded, err := json.Marshal(options); err == nil {
				fields[key] = encoded
			}
		}
	case "containers":
		var hostConfig map[string]any
		key := decodeField(fields, "HostConfig", &hostConfig)
		binds, _ := field(hostConfig, "Binds").([]any)
		for i, bind := range binds {
			bind, _ := bind.(string)
			source, target, _ := strings.Cut(bind, ":")
			if translated := cleanedSource(source); strings.HasPrefix(target, "/") && !strings.Contains(translated, ":") {
				binds[i] = translated + ":" + target
			}
		}
		specs, _ := field(hostConfig, "Mounts").([]any)
		for _, spec := range specs {
			spec, _ := spec.(map[string]any)
			switch field(spec, "Type") {
			case "bind":
				if source, ok := field(spec, "Source").(string); ok {
					spec["Source"] = spelledSource(source)
				}
			case "volume":
				volumeOptions, _ := field(spec, "VolumeOptions").(map[string]any)
				driverConfig, _ := field(volumeOptions, "DriverConfig").(map[string]any)
				translateDevice(field(driverConfig, "Name"), field(driverConfig, "Options"), spelledSource)
			}
		}
		if encoded, err := json.Marshal(hostConfig); err == nil && hostConfig != nil {
			fields[key] = encoded
		}
	}
}

func decodeField(fields map[string]json.RawMessage, name string, value any) string {
	for key, raw := range fields {
		if strings.EqualFold(key, name) {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			_ = decoder.Decode(value) // wrong types stay unset for dockerd to reject
			return key
		}
	}
	return ""
}

func translateDevice(driver, options any, translate func(string) string) {
	optionMap, _ := options.(map[string]any)
	device, ok := optionMap["device"].(string)
	flags, _ := optionMap["o"].(string)
	tokens := strings.Split(flags, ",")
	local := driver == nil || driver == "" || driver == "local"
	if ok && local && (slices.Contains(tokens, "bind") || slices.Contains(tokens, "rbind")) && !slices.Contains(tokens, "remount") {
		optionMap["device"] = translate(device)
	}
}

// field also renames the matched key to name.
func field(object map[string]any, name string) any {
	for key, value := range object {
		if strings.EqualFold(key, name) {
			delete(object, key)
			object[name] = value
			return value
		}
	}
	return nil
}

var (
	requestFields = []string{"hostconfig", "driver", "driveropts"}
	asciiFolds    = strings.NewReplacer("ſ", "s", "K", "k") // the non-ASCII runes strings.EqualFold matches to ASCII letters
)

// dockerd settles repeated names by order, which re-encoding loses.
func hasAmbiguousFields(body []byte) bool {
	type frame struct {
		names     map[string]bool
		key       string
		expectKey bool
		nested    bool
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	stack := []*frame{{}}
	for {
		token, err := decoder.Token()
		if err != nil {
			return !errors.Is(err, io.EOF)
		}
		top := stack[len(stack)-1]
		if name, ok := token.(string); ok && top.expectKey {
			top.key, top.expectKey = strings.ToLower(asciiFolds.Replace(name)), false
			if top.names[top.key] && (top.nested || slices.Contains(requestFields, top.key)) {
				return true
			}
			top.names[top.key] = true
			continue
		}
		top.expectKey = top.names != nil
		nested := top.nested || len(stack) == 2 && slices.Contains(requestFields, top.key)
		switch token {
		case json.Delim('{'):
			stack = append(stack, &frame{names: map[string]bool{}, expectKey: true, nested: nested})
		case json.Delim('['):
			stack = append(stack, &frame{nested: nested})
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
	}
}

// jobMount returns "" also for a path already naming a daemon source.
func jobMount(source string, mounts map[string]string) string {
	target := ""
	for destination, daemonSource := range mounts {
		if daemonSource != "" && (source == daemonSource || strings.HasPrefix(source, daemonSource+"/")) {
			return ""
		}
		if len(destination) > len(target) && (source == destination || strings.HasPrefix(source, destination+"/")) {
			target = destination
		}
	}
	return target
}

type dockerProxyConnKey struct{}

type dockerProxyResponse struct {
	response *http.Response
}

func (r dockerProxyResponse) RoundTrip(_ *http.Request) (*http.Response, error) {
	return r.response, nil
}

// tunnel splices attach and exec streams, which the daemon hijacks with or without an HTTP upgrade
func tunnel(w http.ResponseWriter, r *http.Request, dial func(context.Context, string, string) (net.Conn, error), forward *httputil.ReverseProxy) {
	upstream, err := dial(r.Context(), "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(r.Context(), func() { _ = upstream.Close() })
	defer stop()
	if err := r.Write(upstream); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	reader := bufio.NewReader(upstream)
	var response *http.Response
	for {
		response, err = http.ReadResponse(reader, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if response.StatusCode >= 200 || response.StatusCode == http.StatusSwitchingProtocols {
			break
		}
		maps.Copy(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		clear(w.Header())
		_ = response.Body.Close()
	}
	defer func() {
		_ = upstream.Close()
		_ = response.Body.Close()
	}()
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusSwitchingProtocols && (response.StatusCode != http.StatusOK || mediaType != "application/vnd.docker.raw-stream") {
		ordinary := *forward
		ordinary.Transport = dockerProxyResponse{response: response}
		ordinary.ServeHTTP(w, r)
		return
	}
	downstream, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer downstream.Close()
	if _, err := fmt.Fprintf(buffered, "%s %s\r\n", response.Proto, response.Status); err != nil {
		return
	}
	if err := response.Header.Write(buffered); err != nil {
		return
	}
	if _, err := buffered.WriteString("\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(upstream, io.MultiReader(io.LimitReader(buffered, int64(buffered.Reader.Buffered())), downstream)); err != nil { // Bypass net/http after the prefix so stdin EOF preserves output.
			_ = upstream.Close()
		} else if writer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = writer.CloseWrite()
		} else {
			_ = upstream.Close()
		}
	}()
	_, _ = io.Copy(downstream, reader)
	_ = downstream.Close()
	_ = upstream.Close()
	<-done
}

func RemoveDockerJobResources(ctx context.Context, job string) error {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	return removeLabelled(ctx, cli, job)
}

func removeLabelled(ctx context.Context, cli client.APIClient, job string) error {
	logger := common.Logger(ctx)
	filters := make(client.Filters).Add("label", jobLabel+"="+job)
	containers, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	errs := []error{err}
	for _, c := range containers.Items {
		logger.Infof("removing container %s the job left behind", strings.TrimPrefix(strings.Join(c.Names, ","), "/"))
		errs = append(errs, (&containerReference{cli: cli, id: c.ID}).remove()(ctx))
	}
	networks, err := cli.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	errs = append(errs, err)
	for _, n := range networks.Items {
		if _, err := cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); n.Scope == "swarm" && cerrdefs.IsInvalidArgument(err) { // swarm refuses while a service or its tasks use it
			logger.Infof("keeping network %s, a swarm service still uses it", n.Name)
		} else if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove network %s: %w", n.Name, err))
		}
	}
	volumes, err := cli.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	errs = append(errs, err)
	for _, v := range volumes.Items {
		if _, err := cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove volume %s: %w", v.Name, err))
		}
	}
	return errors.Join(errs...)
}
