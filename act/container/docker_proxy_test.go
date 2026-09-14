// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (m *mockDockerClient) VolumeList(ctx context.Context, opts mobyclient.VolumeListOptions) (mobyclient.VolumeListResult, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(mobyclient.VolumeListResult), args.Error(1)
}

func (m *mockDockerClient) VolumeRemove(ctx context.Context, id string, opts mobyclient.VolumeRemoveOptions) (mobyclient.VolumeRemoveResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.VolumeRemoveResult), args.Error(1)
}

// unix socket paths are limited to about a hundred bytes, which the default macOS TMPDIR exceeds
func shortTempDir(t testing.TB) string {
	t.Helper()
	t.Setenv("TMPDIR", "/tmp")
	return t.TempDir()
}

func daemonSocketPath(t testing.TB, cli mobyclient.APIClient) string {
	t.Helper()
	host := cli.DaemonHost()
	if !strings.HasPrefix(host, "unix://") {
		t.Skipf("skipping: daemon at %s is not a unix socket", host)
	}
	return strings.TrimPrefix(host, "unix://")
}

func TestDockerProxy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket ownership is unavailable on Windows")
	}
	bodies := make(chan []byte, 8)
	createStarted, releaseCreate := make(chan struct{}), make(chan struct{})
	streamsDone := make(chan struct{}, 2)
	daemonSocket := filepath.Join(shortTempDir(t), "d.sock")
	listener, err := net.Listen("unix", daemonSocket)
	require.NoError(t, err)
	daemon := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/create"):
			body, _ := io.ReadAll(r.Body)
			if r.URL.RawQuery == "wait" {
				close(createStarted)
				<-releaseCreate
			}
			bodies <- body
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/exec/detached/start" || r.URL.Path == "/exec/error/start":
			if r.URL.Path == "/exec/error/start" {
				w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
				w.WriteHeader(http.StatusBadRequest)
			}
			_, _ = w.Write([]byte("ordinary"))
		case rawStreamPath.MatchString(r.URL.Path) || r.URL.Path == "/session":
			_, _ = io.Copy(io.Discard, r.Body)
			conn, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			if r.Header.Get("Upgrade") != "" {
				_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nready\n"))
			} else {
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\nready\n"))
			}
			input, _ := io.ReadAll(buffered)
			_, _ = conn.Write(append([]byte("final:"), input...))
			streamsDone <- struct{}{}
		default:
			w.Header().Set("Api-Version", "1.47")
			_, _ = w.Write([]byte("OK " + r.Method + " " + r.URL.Path))
		}
	})}
	go func() { _ = daemon.Serve(listener) }()
	t.Cleanup(func() { _ = daemon.Close() })
	proxy, err := StartDockerProxy(daemonSocket, shortTempDir(t), "job-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxy.SetMounts(map[string]string{"/workspace/o/r": "/volumes/job/_data", "/workspace/o/r/tmp": "", "/var/run/docker.sock": "/tmp/p/docker.sock", "/volumes": "/daemon/volumes"})
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", proxy.Socket)
	}}}
	t.Cleanup(client.CloseIdleConnections)
	dialProxy := func(t *testing.T) *net.UnixConn {
		t.Helper()
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: proxy.Socket, Net: "unix"})
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
		return conn
	}
	startStream := func(t *testing.T, path, upgrade string) (*net.UnixConn, *bufio.Reader) {
		t.Helper()
		conn := dialProxy(t)
		_, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: docker\r\n%sContent-Length: 2\r\n\r\n{}input", path, upgrade)
		require.NoError(t, err)
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = conn.Close()
			_ = resp.Body.Close()
		})
		prefix, err := reader.ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "ready\n", prefix)
		return conn, reader
	}

	t.Run("labels creates", func(t *testing.T) {
		for _, testCase := range []struct{ path, body, want string }{
			{
				"/v1.47/containers/create", `{"Image":"alpine","Unknown":{"enabled":true},"Labels":{"own":"1","com.gitea.runner.job":"other"}}`,
				`{"Image":"alpine","Unknown":{"enabled":true},"Labels":{"own":"1","com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/containers/create", `{"HostConfig":{"Binds":["/workspace/o/r:/src:ro","/workspace/o/r/../r/sub/:/sub","workspace/o/r:/relative","/workspace/o/rest:/rest","/workspace/o/r/tmp/x:/tmp","named:/named","/anonymous","/workspace/o/r:ro","/volumes/job/_data/x:/daemon-path"]},"DriverOpts":{"o":"bind","device":"/workspace/o/r"},"Labels":{"Foo":"1","foo":"2"}}`,
				`{"HostConfig":{"Binds":["/volumes/job/_data:/src:ro","/volumes/job/_data/sub:/sub","workspace/o/r:/relative","/workspace/o/rest:/rest","/workspace/o/r/tmp/x:/tmp","named:/named","/anonymous","/workspace/o/r:ro","/volumes/job/_data/x:/daemon-path"]},"DriverOpts":{"o":"bind","device":"/workspace/o/r"},"Labels":{"Foo":"1","foo":"2","com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/containers/create", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/var/run/docker.sock","Target":"/var/run/docker.sock"},{"type":"bind","source":"/workspace/o/r/sub/","target":"/m"},{"Type":"bind","Source":"/workspace/o/r/sub/../data","Target":"/dotted"},{"Type":"bind","Source":"/workspace/o/r/../r","Target":"/escaping"},{"Type":"BIND","Source":"/workspace/o/r"},{"Type":"volume","Source":"/workspace/o/r"}]}}`,
				`{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/tmp/p/docker.sock","Target":"/var/run/docker.sock"},{"Type":"bind","Source":"/volumes/job/_data/sub/","target":"/m"},{"Type":"bind","Source":"/volumes/job/_data/sub/../data","Target":"/dotted"},{"Type":"bind","Source":"/volumes/job/_data","Target":"/escaping"},{"Type":"BIND","Source":"/workspace/o/r"},{"Type":"volume","Source":"/workspace/o/r"}]},"Labels":{"com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/containers/create", `{"HostConfig":{"Mounts":[{"Type":"volume","VolumeOptions":{"DriverConfig":{"Options":{"o":"bind","device":"/workspace/o/r/data"}}}},{"Type":"volume","VolumeOptions":{"DriverConfig":{"Options":{"o":"unbindable","device":"/workspace/o/r"}}}},{"Type":"volume","VolumeOptions":{"DriverConfig":{"Options":{"o":"bind,remount","device":"/workspace/o/r"}}}},{"Type":"volume","VolumeOptions":{"DriverConfig":{"Name":"plugin","Options":{"o":"bind","device":"/workspace/o/r"}}}}]}}`,
				`{"HostConfig":{"Mounts":[{"Type":"volume","VolumeOptions":{"DriverConfig":{"Options":{"o":"bind","device":"/volumes/job/_data/data"}}}},{"Type":"volume","VolumeOptions":{"DriverConfig":{"Options":{"o":"unbindable","device":"/workspace/o/r"}}}},{"Type":"volume","VolumeOptions":{"DriverConfig":{"Options":{"o":"bind,remount","device":"/workspace/o/r"}}}},{"Type":"volume","VolumeOptions":{"DriverConfig":{"Name":"plugin","Options":{"o":"bind","device":"/workspace/o/r"}}}}]},"Labels":{"com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/containers/create", `{"HostConfig":{"privileged":true,"Privileged":false,"Binds":["/workspace/o/r:/src"]},"hoſtconfig":null}`,
				`{"HostConfig":{"privileged":true,"Privileged":false,"Binds":["/workspace/o/r:/src"]},"hoſtconfig":null,"Labels":{"com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/networks/create", `{"Name":"n"}`,
				`{"Name":"n","Labels":{"com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/volumes/create", `{"Name":"v","labels":null,"DriverOpts":{"type":"none","o":"rbind,ro","device":"/workspace/o/r/"},"HostConfig":{"Binds":["/workspace/o/r:/src"]}}`,
				`{"Name":"v","Labels":{"com.gitea.runner.job":"job-1"},"DriverOpts":{"type":"none","o":"rbind,ro","device":"/volumes/job/_data/"},"HostConfig":{"Binds":["/workspace/o/r:/src"]}}`,
			},
			{
				"/volumes/create", `{"Name":"d","DriverOpts":{"o":"bind","device":1,"device":"/workspace/o/r"}}`,
				`{"Name":"d","DriverOpts":{"o":"bind","device":1,"device":"/workspace/o/r"},"Labels":{"com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/volumes/create", `{"Name":"p","Driver":"plugin","DriverOpts":{"o":"bind","device":"/workspace/o/r"}}`,
				`{"Name":"p","Driver":"plugin","DriverOpts":{"o":"bind","device":"/workspace/o/r"},"Labels":{"com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/volumes/create", "",
				`{"Labels":{"com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/volumes/create", "null",
				`{"Labels":{"com.gitea.runner.job":"job-1"}}`,
			},
			{
				"/volumes/create", `{"Labels":{"discard":"1"},"labels":null,"LABELS":{"own":"1"},"LABELS":{"extra":"2"}}`,
				`{"Labels":{"own":"1","extra":"2","com.gitea.runner.job":"job-1"}}`,
			},
		} {
			resp, err := client.Post("http://docker"+testCase.path, "application/json", strings.NewReader(testCase.body))
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			assert.JSONEq(t, testCase.want, string(<-bodies), testCase.body)
		}
	})

	t.Run("passes other requests through", func(t *testing.T) {
		resp, err := client.Get("http://docker/v1.47/_ping")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "1.47", resp.Header.Get("Api-Version"))
		assert.Equal(t, "OK GET /v1.47/_ping", string(body))
	})

	t.Run("tunnels raw streams through stdin EOF", func(t *testing.T) {
		for _, stream := range []struct{ path, upgrade string }{
			{path: "/v1.47/exec/abc/start"},
			{path: "/containers/abc/attach", upgrade: "Connection: Upgrade\r\nUpgrade: tcp\r\n"},
		} {
			conn, reader := startStream(t, stream.path, stream.upgrade)
			require.NoError(t, conn.CloseWrite())
			output, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Equal(t, "final:input", string(output))
			<-streamsDone
		}
	})

	t.Run("detached and error responses retain keepalive label injection", func(t *testing.T) {
		conn := dialProxy(t)
		reader := bufio.NewReader(conn)
		for endpoint, status := range map[string]int{"detached": http.StatusOK, "error": http.StatusBadRequest} {
			_, err := fmt.Fprintf(conn, "POST /exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 2\r\n\r\n{}", endpoint)
			require.NoError(t, err)
			resp, err := http.ReadResponse(reader, nil)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, status, resp.StatusCode)
			assert.Equal(t, "ordinary", string(body))
			_, err = io.WriteString(conn, "POST /volumes/create HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")
			require.NoError(t, err)
			resp, err = http.ReadResponse(reader, nil)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			assert.JSONEq(t, `{"Labels":{"com.gitea.runner.job":"job-1"}}`, string(<-bodies))
		}
	})

	t.Run("close joins streams and admitted create", func(t *testing.T) {
		t.Cleanup(func() { close(releaseCreate) })
		_, raw := startStream(t, "/exec/live/start", "")
		_, upgraded := startStream(t, "/session", "Connection: Upgrade\r\nUpgrade: tcp\r\n")
		creator := dialProxy(t)
		_, err := io.WriteString(creator, "POST /volumes/create?wait HTTP/1.1\r\nHost: docker\r\nContent-Length: 2\r\n\r\n{}")
		require.NoError(t, err)
		<-createStarted
		require.NoError(t, creator.CloseWrite())
		closed := make(chan error, 1)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		go func() { closed <- proxy.Close(ctx) }()
		for _, reader := range []*bufio.Reader{raw, upgraded} {
			_, err := reader.ReadByte()
			require.ErrorIs(t, err, io.EOF)
			<-streamsDone
		}
		select {
		case err := <-closed:
			t.Fatalf("Close returned before admitted create settled: %v", err)
		default:
		}
		releaseCreate <- struct{}{}
		resp, err := http.ReadResponse(bufio.NewReader(creator), nil)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		assert.JSONEq(t, `{"Labels":{"com.gitea.runner.job":"job-1"}}`, string(<-bodies))
		require.NoError(t, <-closed)
	})
}

func TestRemoveLabelledRemovesContainersNetworksAndVolumes(t *testing.T) {
	containerFailure := errors.New("container removal failed")
	listFailure := errors.New("listing failed")
	volumeFailure := errors.New("volume removal failed")
	ctx := context.Background()
	filters := make(mobyclient.Filters).Add("label", jobLabel+"=job-1")
	cli := &mockDockerClient{}
	cli.On("ContainerList", ctx, mobyclient.ContainerListOptions{All: true, Filters: filters}).
		Return(mobyclient.ContainerListResult{Items: []container.Summary{{ID: "c1", Names: []string{"/app"}}}}, nil).Once()
	cli.On("ContainerKill", ctx, "c1", mock.Anything).Return(mobyclient.ContainerKillResult{}, nil).Once()
	cli.On("ContainerRemove", ctx, "c1", mobyclient.ContainerRemoveOptions{RemoveVolumes: true, Force: true}).
		Return(mobyclient.ContainerRemoveResult{}, containerFailure).Once()
	cli.On("NetworkList", ctx, mobyclient.NetworkListOptions{Filters: filters}).
		Return(mobyclient.NetworkListResult{Items: []network.Summary{{ID: "n1", Name: "stack_default", Scope: "swarm"}}}, listFailure).Once()
	cli.On("NetworkRemove", ctx, "n1", mobyclient.NetworkRemoveOptions{}).Return(mobyclient.NetworkRemoveResult{}, cerrdefs.ErrInvalidArgument).Once()
	cli.On("VolumeList", ctx, mobyclient.VolumeListOptions{Filters: filters}).
		Return(mobyclient.VolumeListResult{Items: []volume.Volume{{Name: "app_data"}}}, nil).Once()
	cli.On("VolumeRemove", ctx, "app_data", mobyclient.VolumeRemoveOptions{}).Return(mobyclient.VolumeRemoveResult{}, volumeFailure).Once()

	err := removeLabelled(ctx, cli, "job-1")
	require.ErrorIs(t, err, containerFailure)
	require.ErrorIs(t, err, listFailure)
	require.ErrorIs(t, err, volumeFailure)
	require.NotErrorIs(t, err, cerrdefs.ErrInvalidArgument)
	require.ErrorContains(t, err, "failed to remove container c1")
	require.ErrorContains(t, err, "failed to remove volume app_data")
	cli.AssertExpectations(t)
}

func TestDockerProxyWithDaemon(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	require.NoError(t, NewDockerPullExecutor(NewDockerPullExecutorInput{Image: "alpine"})(ctx))
	direct, err := GetDockerClient(ctx)
	require.NoError(t, err)
	defer direct.Close()
	dir := shortTempDir(t)
	seen, err := daemonSeesDir(ctx, direct, dir, dir)
	require.NoError(t, err)
	t.Logf("daemon sees the runner's filesystem: %v", seen)

	job := "proxy-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	proxy, err := StartDockerProxy(daemonSocketPath(t, direct), dir, job)
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	viaProxy, err := mobyclient.New(mobyclient.WithHost("unix://" + proxy.Socket))
	require.NoError(t, err)
	defer viaProxy.Close()

	created, err := viaProxy.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config: &container.Config{Image: "alpine", Cmd: []string{"sleep", "300"}, Labels: map[string]string{"own": "1"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = direct.ContainerRemove(ctx, created.ID, mobyclient.ContainerRemoveOptions{Force: true}) })
	_, err = viaProxy.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{})
	require.NoError(t, err)
	net, err := viaProxy.NetworkCreate(ctx, job, mobyclient.NetworkCreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = direct.NetworkRemove(ctx, net.ID, mobyclient.NetworkRemoveOptions{}) })
	_, err = viaProxy.VolumeCreate(ctx, mobyclient.VolumeCreateOptions{Name: job})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = direct.VolumeRemove(ctx, job, mobyclient.VolumeRemoveOptions{Force: true}) })

	inspected, err := direct.ContainerInspect(ctx, created.ID, mobyclient.ContainerInspectOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"own": "1", jobLabel: job}, inspected.Container.Config.Labels)
	vol, err := direct.VolumeInspect(ctx, job, mobyclient.VolumeInspectOptions{})
	require.NoError(t, err)
	assert.Equal(t, job, vol.Volume.Labels[jobLabel])

	exec, err := viaProxy.ExecCreate(ctx, created.ID, mobyclient.ExecCreateOptions{Cmd: []string{"cat"}, AttachStdin: true, AttachStdout: true, TTY: true})
	require.NoError(t, err)
	attached, err := viaProxy.ExecAttach(ctx, exec.ID, mobyclient.ExecAttachOptions{TTY: true})
	require.NoError(t, err)
	_, err = attached.Conn.Write([]byte("hello\n"))
	require.NoError(t, err)
	echoed, err := attached.Reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "hello\r\n", echoed)
	attached.Close()

	require.NoError(t, proxy.Close(ctx))
	require.NoError(t, RemoveDockerJobResources(ctx, job))
	_, err = direct.ContainerInspect(ctx, created.ID, mobyclient.ContainerInspectOptions{})
	assert.True(t, cerrdefs.IsNotFound(err))
	_, err = direct.NetworkInspect(ctx, net.ID, mobyclient.NetworkInspectOptions{})
	assert.True(t, cerrdefs.IsNotFound(err))
	_, err = direct.VolumeInspect(ctx, job, mobyclient.VolumeInspectOptions{})
	assert.True(t, cerrdefs.IsNotFound(err))
}

func BenchmarkDockerProxy(b *testing.B) {
	ctx := context.Background()
	direct, err := GetDockerClient(ctx)
	require.NoError(b, err)
	defer direct.Close()
	if _, err := direct.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		b.Skipf("docker daemon unreachable: %v", err)
	}
	proxy, err := StartDockerProxy(daemonSocketPath(b, direct), shortTempDir(b), "bench")
	require.NoError(b, err)
	defer func() { _ = proxy.Close(ctx) }()
	viaProxy, err := mobyclient.New(mobyclient.WithHost("unix://" + proxy.Socket))
	require.NoError(b, err)
	defer viaProxy.Close()

	require.NoError(b, NewDockerPullExecutor(NewDockerPullExecutorInput{Image: "alpine"})(ctx))
	created, err := direct.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{Config: &container.Config{Image: "alpine", Cmd: []string{"sleep", "600"}}})
	require.NoError(b, err)
	defer func() { _, _ = direct.ContainerRemove(ctx, created.ID, mobyclient.ContainerRemoveOptions{Force: true}) }()
	_, err = direct.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{})
	require.NoError(b, err)

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	payload := make([]byte, 64<<20)
	require.NoError(b, writer.WriteHeader(&tar.Header{Name: "blob", Mode: 0o600, Size: int64(len(payload))}))
	_, _ = writer.Write(payload)
	require.NoError(b, writer.Close())

	for name, cli := range map[string]mobyclient.APIClient{"direct": direct, "proxy": viaProxy} {
		b.Run("ping/"+name, func(b *testing.B) {
			for b.Loop() {
				if _, err := cli.Ping(ctx, mobyclient.PingOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("copy64MiB/"+name, func(b *testing.B) {
			b.SetBytes(int64(archive.Len()))
			for b.Loop() {
				_, err := cli.CopyToContainer(ctx, created.ID, mobyclient.CopyToContainerOptions{DestinationPath: "/tmp", Content: bytes.NewReader(archive.Bytes())})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type probeClient struct {
	mobyclient.APIClient
	create func(mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error)
	remove func(context.Context, string, mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error)
}

func (c *probeClient) ImageList(context.Context, mobyclient.ImageListOptions) (mobyclient.ImageListResult, error) {
	return mobyclient.ImageListResult{Items: []image.Summary{{ID: "probe-image"}}}, nil
}

func (c *probeClient) ContainerCreate(_ context.Context, opts mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
	return c.create(opts)
}

func (c *probeClient) ContainerRemove(ctx context.Context, id string, opts mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	return c.remove(ctx, id, opts)
}

func TestDaemonSeesDir(t *testing.T) {
	dir := t.TempDir()
	markers := make(map[string]bool)
	for _, testCase := range []struct {
		name      string
		private   bool
		removeErr error
	}{
		{name: "cleanup after cancellation"},
		{name: "private filesystem with stale marker", private: true},
		{name: "cleanup error after cancellation", removeErr: errors.New("cleanup failed")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			daemonDir := dir
			if testCase.private {
				daemonDir = t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(daemonDir, "probe"), []byte("stale"), 0o600))
			}
			removed := false
			cli := &probeClient{
				create: func(opts mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
					require.Len(t, opts.HostConfig.Mounts, 1)
					marker := opts.HostConfig.Mounts[0].Source
					assert.Equal(t, mount.Mount{Type: mount.TypeBind, Source: marker, Target: "/gitea-runner-probe", ReadOnly: true}, opts.HostConfig.Mounts[0])
					assert.False(t, markers[marker])
					markers[marker] = true
					info, err := os.Stat(marker)
					require.NoError(t, err)
					assert.True(t, info.Mode().IsRegular())
					cancel()
					if _, err := os.Stat(filepath.Join(daemonDir, filepath.Base(marker))); errors.Is(err, os.ErrNotExist) {
						return mobyclient.ContainerCreateResult{}, cerrdefs.ErrInvalidArgument
					}
					return mobyclient.ContainerCreateResult{ID: "probe"}, nil
				},
				remove: func(ctx context.Context, id string, opts mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
					removed = true
					assert.Equal(t, "probe", id)
					assert.Equal(t, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}, opts)
					require.NoError(t, ctx.Err())
					_, bounded := ctx.Deadline()
					assert.True(t, bounded)
					return mobyclient.ContainerRemoveResult{}, testCase.removeErr
				},
			}
			seen, err := daemonSeesDir(ctx, cli, dir, dir)
			require.ErrorIs(t, err, testCase.removeErr)
			assert.Equal(t, !testCase.private && testCase.removeErr == nil, seen)
			assert.Equal(t, !testCase.private, removed)
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}
