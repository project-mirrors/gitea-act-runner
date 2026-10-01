// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"

	"github.com/kballard/go-shellquote"
	"github.com/spf13/pflag"
)

// KubernetesOptions configures the pod a job runs in.
type KubernetesOptions struct {
	Kubeconfig   string
	Namespace    string
	PodTemplates []map[string]any // merged in order, a later one winning
	MaxLifetime  time.Duration
	ForcePull    bool
	RunnerUUID   string
}

// KubernetesPod runs a job container and its service containers as one pod.
type KubernetesPod struct {
	options          KubernetesOptions
	client           *kubeClient
	name, secretName string
	job              *kubernetesContainer
	services         []*kubernetesContainer
	arch, path       string
}

type kubernetesContainer struct {
	LinuxContainerEnvironmentExtensions
	pod   *KubernetesPod
	id    string // the workflow's service id, name is its DNS label form
	name  string
	input *NewContainerInput
	probe map[string]any
}

type containerStatus struct {
	Name  string
	Ready bool
	State struct {
		Waiting    *struct{ Reason, Message string }
		Running    *struct{}
		Terminated *struct {
			ExitCode int
			Message  string
		}
	}
}

type podStatus struct {
	Spec   struct{ Containers []struct{} }
	Status struct {
		Phase, Reason, Message                   string
		Conditions                               []struct{ Type, Status, Message string }
		InitContainerStatuses, ContainerStatuses []containerStatus
	}
}

func NewKubernetesPod(options KubernetesOptions) (*KubernetesPod, error) {
	client, err := newKubeClient(options.Kubeconfig, options.Namespace)
	if err != nil {
		return nil, fmt.Errorf("connect to Kubernetes: %w", err)
	}
	return &KubernetesPod{options: options, client: client}, nil
}

// JobContainer returns the container of the pod the job's steps run in.
func (p *KubernetesPod) JobContainer(input *NewContainerInput) ExecutionsEnvironment {
	p.job = &kubernetesContainer{pod: p, name: "job", input: input}
	return p.job
}

var invalidLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)

// KubernetesServiceName returns the container name and hostname of service id, made a DNS label as both must be.
func KubernetesServiceName(id string) string {
	name := strings.Trim(invalidLabelChars.ReplaceAllString(strings.ToLower(id), "-"), "-")
	return strings.TrimRight(name[:min(63, len(name))], "-")
}

// ServiceContainer returns a container of the pod the job reaches at hostname KubernetesServiceName(id).
func (p *KubernetesPod) ServiceContainer(id string, input *NewContainerInput) ExecutionsEnvironment {
	service := &kubernetesContainer{pod: p, id: id, name: KubernetesServiceName(id), input: input}
	p.services = append(p.services, service)
	return service
}

func (p *KubernetesPod) create(ctx context.Context) error {
	prefix := strings.Trim(strings.ToLower(p.job.input.Name), "-")
	prefix = cmp.Or(strings.TrimRight(prefix[:min(40, len(prefix))], "-"), "gitea-runner") + "-"
	name := prefix + strings.ToLower(rand.Text()[:8]) // chosen here, so cleanup knows it even when a create response is lost
	labels := map[string]any{"app.kubernetes.io/managed-by": "gitea-runner"}
	if p.options.RunnerUUID != "" {
		labels[runnerUUIDLabel] = p.options.RunnerUUID
	}
	metadata := map[string]any{"name": name, "labels": labels}
	if hostname, err := os.Hostname(); p.client.inCluster && err == nil {
		var runner struct{ Metadata struct{ Name, UID string } }
		if err := p.client.do(ctx, http.MethodGet, p.client.path("pods/"+hostname), nil, &runner); err == nil {
			metadata["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "name": runner.Metadata.Name, "uid": runner.Metadata.UID}}
		} else {
			common.Logger(ctx).Debugf("The runner is not pod %s, job pods get no owner: %v", hostname, err)
		}
	}
	secretData := map[string]string{}
	manifest, err := p.manifest(name, secretData, metadata)
	if err != nil {
		return err
	}
	for _, service := range p.services {
		if !strings.EqualFold(service.id, service.name) {
			common.Logger(ctx).Warnf("Service %s is reachable as %s or localhost, as a Kubernetes hostname cannot be %q", service.id, service.name, service.id)
		}
	}
	if len(secretData) > 0 {
		p.secretName = name // named like the pod, RemoveOrphanKubernetesResources finds it by that
		if err := p.client.do(ctx, http.MethodPost, p.client.path("secrets"), map[string]any{"metadata": metadata, "stringData": secretData}, nil); err != nil {
			return fmt.Errorf("create secret: %w", err)
		}
	}
	p.name = name
	if err := p.client.do(ctx, http.MethodPost, p.client.path("pods"), manifest, nil); err != nil {
		return fmt.Errorf("create pod: %w", err)
	}
	return nil
}

func (p *KubernetesPod) manifest(secret string, secretData map[string]string, metadata map[string]any) (any, error) {
	template := map[string]any{"spec": map[string]any{
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"terminationGracePeriodSeconds": 0, // the job container's tail ignores SIGTERM
	}}
	for _, layer := range p.options.PodTemplates {
		template, _ = mergeManifest(template, layer, "").(map[string]any)
	}
	templateSpec, _ := template["spec"].(map[string]any)
	templateContainers, _ := templateSpec["containers"].([]any)
	templateVolumes, _ := templateSpec["volumes"].([]any)
	var serviceTemplate any
	if index := slices.IndexFunc(templateContainers, func(container any) bool { return manifestField(container, "name") == "$services" }); index >= 0 {
		serviceTemplate = templateContainers[index]
		templateSpec["containers"] = slices.Delete(slices.Clone(templateContainers), index, index+1)
	}
	var containers, hostnames, volumes []any
	names := map[string]bool{}
	for _, container := range templateContainers {
		names[manifestField(container, "name")] = true
	}
	for _, c := range append([]*kubernetesContainer{p.job}, p.services...) {
		if c != p.job && (c.name == "" || names[c.name]) {
			return nil, fmt.Errorf("service %s maps to container %q, which is empty or named like another container of the pod", c.id, c.name)
		}
		names[c.name] = true
		spec, err := c.spec(secret, secretData)
		if err != nil {
			return nil, err
		}
		if c == p.job {
			containers = append(containers, spec)
			continue
		}
		containers = append(containers, mergeManifest(serviceTemplate, spec, ""))
		hostnames = append(hostnames, c.name)
	}
	for _, name := range []string{"workspace", "act"} {
		if !slices.ContainsFunc(templateVolumes, func(volume any) bool { return manifestField(volume, "name") == name }) {
			volumes = append(volumes, map[string]any{"name": name, "emptyDir": map[string]any{}})
		}
	}
	spec := map[string]any{"restartPolicy": "Never", "containers": containers, "volumes": volumes}
	if len(hostnames) > 0 {
		spec["hostAliases"] = []any{map[string]any{"ip": "127.0.0.1", "hostnames": hostnames}}
	}
	if p.options.MaxLifetime > 0 {
		spec["activeDeadlineSeconds"] = max(1, int64(p.options.MaxLifetime.Seconds()))
	}
	return mergeManifest(template, map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata, "spec": spec}, ""), nil
}

// mergeManifest merges over onto base like a strategic merge patch: maps key by key, map list items by name or volumeMounts by mountPath, env entries, volumes and other values replaced.
func mergeManifest(base, over any, field string) any {
	switch over := over.(type) {
	case map[string]any:
		merged := map[string]any{}
		if base, ok := base.(map[string]any); ok {
			maps.Copy(merged, base)
		}
		for key, value := range over {
			merged[key] = mergeManifest(merged[key], value, key)
		}
		return merged
	case []any:
		key := cmp.Or(map[string]string{"volumeMounts": "mountPath"}[field], "name")
		baseList, _ := base.([]any)
		merged := slices.Clone(baseList)
		for _, item := range over {
			if _, ok := item.(map[string]any); !ok {
				return over
			}
			name := manifestField(item, key)
			index := slices.IndexFunc(merged, func(existing any) bool { return name != "" && manifestField(existing, key) == name })
			switch {
			case index < 0:
				merged = append(merged, item)
			case field == "env" || field == "volumes": // each holds one source, merging two makes a pod the API rejects
				merged[index] = item
			default:
				merged[index] = mergeManifest(merged[index], item, "")
			}
		}
		return merged
	}
	return over
}

func manifestField(item any, key string) string {
	fields, _ := item.(map[string]any)
	value, _ := fields[key].(string)
	return value
}

func (c *kubernetesContainer) spec(secret string, secretData map[string]string) (map[string]any, error) {
	env := []any{}
	for index, entry := range c.input.Env {
		key, value, _ := strings.Cut(entry, "=")
		if c == c.pod.job {
			env = append(env, map[string]any{"name": key, "value": value})
			continue
		}
		secretKey := fmt.Sprintf("%s.%d", c.name, index)
		secretData[secretKey] = value
		env = append(env, map[string]any{"name": key, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": secretKey}}})
	}
	spec := map[string]any{
		"name": c.name, "image": c.input.Image, "command": escapeExpansion(c.input.Entrypoint), "args": escapeExpansion(c.input.Cmd),
		"workingDir": c.input.WorkingDir, "env": env,
	}
	if c.pod.options.ForcePull {
		spec["imagePullPolicy"] = "Always"
	}
	if c == c.pod.job {
		spec["volumeMounts"] = []any{
			map[string]any{"name": "workspace", "mountPath": c.input.WorkingDir},
			map[string]any{"name": "act", "mountPath": c.GetActPath()},
		}
	}
	if err := c.parseOptions(); err != nil {
		return nil, fmt.Errorf("service %s: %w", c.name, err)
	}
	if c.probe != nil {
		spec["startupProbe"] = c.probe
	}
	return spec, nil
}

// escapeExpansion keeps kubelet from expanding $(VAR) and $$ in command and args, which docker passes as is.
func escapeExpansion(args []string) []string {
	args = slices.Clone(args)
	for index := range args {
		args[index] = strings.ReplaceAll(args[index], "$", "$$")
	}
	return args
}

// parseOptions turns docker health options, the only ones a pod supports, into a startup probe, which like docker gives up after the retries.
func (c *kubernetesContainer) parseOptions() error {
	args, err := shellquote.Split(c.input.WorkflowOptions)
	if err != nil {
		return err
	}
	flags := pflag.NewFlagSet("options", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	command := flags.String("health-cmd", "", "")
	interval := flags.Duration("health-interval", 0, "")
	timeout := flags.Duration("health-timeout", 0, "")
	retries := flags.Int("health-retries", 0, "")
	startPeriod := flags.Duration("health-start-period", 0, "")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("options %q: %w, only --health-* options are supported on Kubernetes", c.input.WorkflowOptions, err)
	}
	if *command == "" {
		return nil
	}
	period := max(1, int(cmp.Or(*interval, 30*time.Second).Seconds()))
	c.probe = map[string]any{
		"exec":             map[string]any{"command": []string{"/bin/sh", "-c", *command}},
		"periodSeconds":    period,
		"timeoutSeconds":   max(1, int(cmp.Or(*timeout, 30*time.Second).Seconds())),
		"failureThreshold": max(1, cmp.Or(*retries, 3)) + int(startPeriod.Seconds())/period, // docker does not count failures in the start period
	}
	return nil
}

func (p *KubernetesPod) get(ctx context.Context) (*podStatus, error) {
	var pod podStatus
	return &pod, p.client.do(ctx, http.MethodGet, p.client.path("pods/"+p.name), nil, &pod)
}

func (p *KubernetesPod) waitStarted(ctx context.Context) error {
	var unscheduled string
	for delay := 100 * time.Millisecond; ; delay = min(2*delay, time.Second) {
		pod, err := p.get(ctx)
		if err != nil {
			return err
		}
		if pod.Status.Phase == "Failed" {
			return fmt.Errorf("pod %s failed: %s", p.name, cmp.Or(pod.Status.Message, pod.Status.Reason))
		}
		started := len(pod.Status.ContainerStatuses) == len(pod.Spec.Containers)
		for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			if waiting := status.State.Waiting; waiting != nil {
				switch waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
					return fmt.Errorf("container %s of pod %s: %s: %s", status.Name, p.name, waiting.Reason, waiting.Message)
				}
				started = false
			}
		}
		if started {
			return nil
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "PodScheduled" && condition.Status == "False" && condition.Message != unscheduled {
				unscheduled = condition.Message
				common.Logger(ctx).Infof("Waiting for pod %s to be scheduled: %s", p.name, unscheduled)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (c *kubernetesContainer) Create(_, _ []string) common.Executor {
	return func(ctx context.Context) error {
		if c != c.pod.job {
			return nil
		}
		return c.pod.create(ctx)
	}
}

func (c *kubernetesContainer) Start(_ bool) common.Executor {
	return func(ctx context.Context) error {
		if c != c.pod.job {
			return nil
		}
		if err := c.pod.waitStarted(ctx); err != nil {
			return err
		}
		var stdout bytes.Buffer
		if err := c.pod.client.exec(ctx, c.pod.name, c.name, []string{"sh", "-c", `printf '%s\n%s' "$(uname -m)" "$PATH"`}, nil, &stdout, io.Discard); err != nil {
			return err
		}
		machine, path, _ := strings.Cut(stdout.String(), "\n")
		c.pod.arch, c.pod.path = goArchToActionArch(machine), path
		return nil
	}
}

func (*kubernetesContainer) Pull(bool) common.Executor {
	return func(context.Context) error { return nil }
}

var (
	shellName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	bashReadonly = []string{"BASHOPTS", "BASH_VERSINFO", "EUID", "PPID", "SHELLOPTS", "UID"} // bash as sh cannot export these
)

// Exec feeds the command to sh on stdin, which exports the env so it stays out of the request URL and, where sh can export a name, out of process arguments.
func (c *kubernetesContainer) Exec(command []string, env map[string]string, _, workdir string) common.Executor {
	return func(ctx context.Context) error {
		script, args := "", []string{"env", "--"}
		for name, value := range env {
			if shellName.MatchString(name) && !slices.Contains(bashReadonly, name) {
				script += "command export " + shellquote.Join(name+"="+value) + "\n"
			} else {
				args = append(args, name+"="+value)
			}
		}
		script += "exec " + shellquote.Join(append(args, command...)...)
		if workdir != "" {
			script = "cd " + shellquote.Join(workdir) + " || exit\n" + script
		}
		defer common.FlushWriter(c.input.Stdout)
		defer common.FlushWriter(c.input.Stderr)
		return c.pod.client.exec(ctx, c.pod.name, c.name, []string{"sh"}, strings.NewReader(script), c.input.Stdout, c.input.Stderr)
	}
}

func (c *kubernetesContainer) extract(ctx context.Context, destPath string, write func(io.Writer) error) error {
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() { writer.CloseWithError(write(writer)) }()
	var stderr bytes.Buffer
	if err := c.pod.client.exec(ctx, c.pod.name, c.name, []string{"sh", "-c", `mkdir -p "$1" && tar -xf - -C "$1"`, "sh", destPath}, reader, io.Discard, &stderr); err != nil {
		return fmt.Errorf("extract to %s: %w: %s", destPath, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *kubernetesContainer) Copy(destPath string, files ...*FileEntry) common.Executor {
	return func(ctx context.Context) error {
		return c.extract(ctx, destPath, func(writer io.Writer) error { return writeFilesTar(ctx, writer, 0, 0, files...) })
	}
}

func (c *kubernetesContainer) CopyDir(destPath, srcPath string, useGitIgnore, skipGitDir bool) common.Executor {
	return func(ctx context.Context) error {
		return c.extract(ctx, "/", func(writer io.Writer) error {
			return writeDirTar(ctx, writer, destPath, srcPath, useGitIgnore, skipGitDir, 0, 0)
		})
	}
}

// GetContainerArchive streams the archive, checking the path first as tar writes an empty archive for a missing one.
func (c *kubernetesContainer) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	go func() {
		var stderr bytes.Buffer
		err := c.pod.client.exec(ctx, c.pod.name, c.name, []string{"sh", "-c", `test -e "$1" -o -L "$1" && exec tar -cf - -C "$2" "$3"`, "sh", srcPath, path.Dir(srcPath), path.Base(srcPath)}, nil, writer, &stderr)
		if err != nil {
			err = fmt.Errorf("archive %s: %w: %s", srcPath, err, strings.TrimSpace(stderr.String()))
		}
		writer.CloseWithError(err)
	}()
	buffered := bufio.NewReader(reader)
	if _, err := buffered.Peek(1); err != nil {
		reader.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{buffered, reader}, nil
}

func (c *kubernetesContainer) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return parseEnvFile(c, srcPath, env)
}

func (c *kubernetesContainer) UpdateFromImageEnv(env *map[string]string) common.Executor {
	return func(context.Context) error {
		(*env)["PATH"] = cmp.Or((*env)["PATH"], c.pod.path)
		return nil
	}
}

func (c *kubernetesContainer) Inspect(ctx context.Context) (*Info, error) {
	if c.pod.name == "" {
		return nil, fmt.Errorf("container %s %w", c.name, ErrContainerNotFound)
	}
	pod, err := c.pod.get(ctx)
	if isNotFound(err) {
		return nil, fmt.Errorf("pod %s %w", c.pod.name, ErrContainerNotFound)
	} else if err != nil {
		return nil, err
	}
	info := &Info{ID: c.pod.name + "/" + c.name, State: "created", Health: HealthNone, Ports: map[string]string{}}
	for port := range c.input.ExposedPorts {
		info.Ports[port.Port()] = port.Port() // services share the pod's network
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != c.name {
			continue
		}
		if status.State.Running != nil {
			info.State = StateRunning
		} else if terminated := status.State.Terminated; terminated != nil {
			info.State, info.ExitCode, info.HealthOutput = "exited", terminated.ExitCode, terminated.Message
		}
		if c.probe != nil {
			switch {
			case status.Ready:
				info.Health = HealthHealthy
			case info.State == "exited":
				info.Health = HealthUnhealthy
			default:
				info.Health = HealthStarting
			}
		}
	}
	return info, nil
}

func (c *kubernetesContainer) DumpLogs(ctx context.Context) error {
	if c.pod.name == "" {
		return fmt.Errorf("container %s %w", c.name, ErrContainerNotFound)
	}
	defer common.FlushWriter(c.input.Stdout)
	return c.pod.client.do(ctx, http.MethodGet, c.pod.client.path("pods/"+c.pod.name+"/log?container="+c.name), nil, c.input.Stdout)
}

func (c *kubernetesContainer) Remove() common.Executor {
	return func(ctx context.Context) error {
		if c != c.pod.job {
			return nil
		}
		return errors.Join(c.pod.client.delete(ctx, "pods", c.pod.name), c.pod.client.delete(ctx, "secrets", c.pod.secretName))
	}
}

// RemoveOrphanKubernetesResources removes this runner's job pods created before createdBefore and the Secrets named like them, as listing Secrets would reveal all of the namespace's.
func RemoveOrphanKubernetesResources(ctx context.Context, options KubernetesOptions, createdBefore time.Time) error {
	client, err := newKubeClient(options.Kubeconfig, options.Namespace)
	if err != nil {
		return fmt.Errorf("connect to Kubernetes: %w", err)
	}
	defer client.http.CloseIdleConnections()
	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string
				CreationTimestamp time.Time
			}
		}
	}
	if err := client.do(ctx, http.MethodGet, client.path("pods?labelSelector="+url.QueryEscape(runnerUUIDLabel+"="+options.RunnerUUID)), nil, &pods); err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	var errs []error
	for _, item := range pods.Items {
		if !item.Metadata.CreationTimestamp.Before(createdBefore) {
			continue
		}
		if err := client.delete(ctx, "secrets", item.Metadata.Name); err != nil {
			errs = append(errs, err) // leave the pod for the next cleanup, it is the only way back to its Secret
			continue
		}
		errs = append(errs, client.delete(ctx, "pods", item.Metadata.Name))
	}
	return errors.Join(errs...)
}

func (c *kubernetesContainer) Close() common.Executor {
	return func(context.Context) error {
		c.pod.client.http.CloseIdleConnections()
		return nil
	}
}

func (c *kubernetesContainer) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	oldStdout, oldStderr := c.input.Stdout, c.input.Stderr
	c.input.Stdout, c.input.Stderr = stdout, stderr
	return oldStdout, oldStderr
}

func (c *kubernetesContainer) GetRunnerContext(_ context.Context) map[string]any {
	return map[string]any{"os": "Linux", "arch": c.pod.arch, "temp": "/tmp", "tool_cache": DefaultToolCache}
}
