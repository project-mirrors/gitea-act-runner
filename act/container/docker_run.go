// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/filecollector"

	"dario.cat/mergo"
	"github.com/bmatcuk/doublestar/v4"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/compose/loader"
	"github.com/docker/cli/cli/connhelper"
	"github.com/go-git/go-billy/v5/helper/polyfill"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/joho/godotenv"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

// drainGracePeriod bounds how long we wait for an output-copy goroutine to
// finish draining a container's output before returning, so that neither a
// cancellation (waitForCommand) nor a normal container exit (wait) truncates
// the tail of the log. It is a safety bound: in the common case the stream
// reaches EOF and the goroutine returns well before this elapses.
const drainGracePeriod = 2 * time.Second

// NewContainer creates a reference to a container
func NewContainer(input *NewContainerInput) ExecutionsEnvironment {
	cr := new(containerReference)
	cr.input = input
	// Resolved up front because the image pull runs before the container is created.
	cf := createFlagsFromOptions(input.allOptions())
	if cf.platform != "" {
		cr.input.Platform = cf.platform
	}
	cr.pullPolicy = cf.pull
	return cr
}

// supportsContainerImagePlatform reports whether the Docker server API version
// is 1.41 and beyond
func supportsContainerImagePlatform(ctx context.Context, cli client.APIClient) (bool, error) {
	ver, err := cli.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return false, fmt.Errorf("get docker API version: %w", err)
	}
	return versions.GreaterThanOrEqualTo(ver.APIVersion, "1.41"), nil
}

func (cr *containerReference) Create(capAdd, capDrop []string) common.Executor {
	return common.
		NewInfoExecutor("docker create image=%s platform=%s entrypoint=%+q cmd=%+q network=%+q", cr.input.Image, cr.input.Platform, cr.input.Entrypoint, cr.input.Cmd, cr.input.NetworkMode).
		Then(
			common.NewPipelineExecutor(
				cr.connect(),
				cr.find(),
				cr.create(capAdd, capDrop),
			).IfNot(common.Dryrun),
		)
}

func (cr *containerReference) Start(attach bool) common.Executor {
	return common.
		NewInfoExecutor("docker run image=%s platform=%s entrypoint=%+q cmd=%+q network=%+q", cr.input.Image, cr.input.Platform, cr.input.Entrypoint, cr.input.Cmd, cr.input.NetworkMode).
		Then(
			common.NewPipelineExecutor(
				cr.connect(),
				cr.find(),
				cr.attach().IfBool(attach),
				cr.start(),
				cr.wait().IfBool(attach),
				cr.tryReadUID(),
				cr.tryReadGID(),
				func(ctx context.Context) error {
					// If this fails, then folders have wrong permissions on non root container
					if cr.UID != 0 || cr.GID != 0 {
						_ = cr.Exec([]string{"chown", "-R", fmt.Sprintf("%d:%d", cr.UID, cr.GID), cr.input.WorkingDir}, nil, "0", "")(ctx)
					}
					return nil
				},
			).IfNot(common.Dryrun),
		)
}

func (cr *containerReference) Pull(forcePull bool) common.Executor {
	if cr.pullPolicy == pullPolicyNever {
		return common.NewInfoExecutor("docker pull skipped image=%s, --pull=never in the options", cr.input.Image)
	}
	forcePull = forcePull || cr.pullPolicy == pullPolicyAlways

	return common.
		NewInfoExecutor("docker pull image=%s platform=%s username=%s forcePull=%t", cr.input.Image, cr.input.Platform, cr.input.Username, forcePull).
		Then(
			NewDockerPullExecutor(NewDockerPullExecutorInput{
				Image:     cr.input.Image,
				ForcePull: forcePull,
				Platform:  cr.input.Platform,
				Username:  cr.input.Username,
				Password:  cr.input.Password,
			}),
		)
}

func (cr *containerReference) Copy(destPath string, files ...*FileEntry) common.Executor {
	return common.NewPipelineExecutor(
		cr.connect(),
		cr.find(),
		cr.copyContent(destPath, files...),
	).IfNot(common.Dryrun)
}

func (cr *containerReference) CopyDir(destPath, srcPath string, useGitIgnore, skipGitDir bool) common.Executor {
	return common.NewPipelineExecutor(
		common.NewInfoExecutor("docker cp src=%s dst=%s", srcPath, destPath),
		cr.connect(),
		cr.find(),
		cr.copyDir(destPath, srcPath, useGitIgnore, skipGitDir),
		func(ctx context.Context) error {
			// If this fails, then folders have wrong permissions on non root container
			if cr.UID != 0 || cr.GID != 0 {
				_ = cr.Exec([]string{"chown", "-R", fmt.Sprintf("%d:%d", cr.UID, cr.GID), destPath}, nil, "0", "")(ctx)
			}
			return nil
		},
	).IfNot(common.Dryrun)
}

func (cr *containerReference) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	if common.Dryrun(ctx) {
		return nil, errors.New("DRYRUN is not supported in GetContainerArchive")
	}
	// Direct entry point (no pipeline) — revalidate cr.id ourselves.
	if err := cr.connect()(ctx); err != nil {
		return nil, err
	}
	if err := cr.find()(ctx); err != nil {
		return nil, err
	}
	if cr.id == "" {
		return nil, cr.missingContainerError("get archive %s", srcPath)
	}
	result, err := cr.cli.CopyFromContainer(ctx, cr.id, client.CopyFromContainerOptions{SourcePath: srcPath})
	if err != nil {
		return nil, err
	}
	return result.Content, nil
}

// Inspect resolves the container by name when its id is not known yet. One the daemon no
// longer knows is reported as ErrContainerNotFound.
func (cr *containerReference) Inspect(ctx context.Context) (*Info, error) {
	if common.Dryrun(ctx) {
		return &Info{Health: HealthNone, Ports: map[string]string{}}, nil
	}
	if err := cr.connect()(ctx); err != nil {
		return nil, err
	}
	if cr.id == "" { // a known id is trusted, find() would spend a call validating it
		if err := cr.find()(ctx); err != nil {
			return nil, err
		}
	}
	if cr.id == "" {
		return nil, cr.missingContainerError("inspect it")
	}

	result, err := cr.cli.ContainerInspect(ctx, cr.id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil, cr.missingContainerError("inspect it")
	} else if err != nil {
		return nil, err
	}
	return containerInfoFromInspect(result.Container), nil
}

// DumpLogs copies the container's log so far to its output writers.
func (cr *containerReference) DumpLogs(ctx context.Context) error {
	if common.Dryrun(ctx) {
		return nil
	}
	if err := cr.connect()(ctx); err != nil {
		return err
	}
	if cr.id == "" {
		return cr.missingContainerError("read its logs")
	}

	logs, err := cr.cli.ContainerLogs(ctx, cr.id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err
	}
	defer logs.Close()
	return cr.copyOutput(logs)
}

// copyOutput writes a container stream to the writers the container was created with,
// demultiplexing it unless the container has a TTY, which sends a single raw stream.
func (cr *containerReference) copyOutput(stream io.Reader) error {
	outWriter := cr.input.Stdout
	if outWriter == nil {
		outWriter = os.Stdout
	}
	errWriter := cr.input.Stderr
	if errWriter == nil {
		errWriter = os.Stderr
	}

	var err error
	if !cr.input.AllocatePTY || os.Getenv("NORAW") != "" {
		_, err = stdcopy.StdCopy(outWriter, errWriter, stream)
	} else {
		_, err = io.Copy(outWriter, stream)
	}
	// Flush any buffered, not-yet-newline-terminated trailing line so the final line of
	// the output is not lost when it is not newline-terminated.
	common.FlushWriter(outWriter)
	common.FlushWriter(errWriter)
	return err
}

func containerInfoFromInspect(inspect container.InspectResponse) *Info {
	info := &Info{
		ID:     inspect.ID,
		Health: HealthNone,
		Ports:  map[string]string{}, // an empty map, never null, in the expression context
		Mounts: make(map[string]string, len(inspect.Mounts)),
	}
	for _, mountPoint := range inspect.Mounts {
		info.Mounts[mountPoint.Destination] = mountPoint.Source
	}
	if hostConfig := inspect.HostConfig; hostConfig != nil { // Mounts omits --tmpfs targets and subpaths
		for target := range hostConfig.Tmpfs {
			info.Mounts[path.Clean(target)] = ""
		}
		for _, spec := range hostConfig.Mounts {
			subpath := ""
			if spec.VolumeOptions != nil {
				subpath = spec.VolumeOptions.Subpath
			} else if spec.ImageOptions != nil {
				subpath = spec.ImageOptions.Subpath
			}
			if target := path.Clean(spec.Target); subpath != "" && info.Mounts[target] != "" {
				info.Mounts[target] = path.Join(info.Mounts[target], subpath)
			}
		}
	}
	for _, target := range []string{"/etc/hosts", "/etc/hostname", "/etc/resolv.conf"} { // specific to the job's network namespace
		if _, mounted := info.Mounts[target]; !mounted {
			info.Mounts[target] = ""
		}
	}

	if state := inspect.State; state != nil {
		info.State = string(state.Status)
		info.ExitCode = state.ExitCode
		if health := state.Health; health != nil {
			info.Health = string(health.Status)
			if len(health.Log) > 0 {
				info.HealthOutput = strings.TrimSpace(health.Log[len(health.Log)-1].Output)
			}
		}
	}

	if settings := inspect.NetworkSettings; settings != nil {
		for port, bindings := range settings.Ports {
			for _, binding := range bindings { // the last binding wins, a port maps to one host port
				if binding.HostPort != "" {
					info.Ports[port.Port()] = binding.HostPort
				}
			}
		}
	}

	return info
}

func (cr *containerReference) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return parseEnvFile(cr, srcPath, env).IfNot(common.Dryrun)
}

func (cr *containerReference) UpdateFromImageEnv(env *map[string]string) common.Executor {
	return cr.extractFromImageEnv(env).IfNot(common.Dryrun)
}

func (cr *containerReference) Exec(command []string, env map[string]string, user, workdir string) common.Executor {
	return common.NewPipelineExecutor(
		common.NewInfoExecutor("docker exec cmd=[%s] user=%s workdir=%s", strings.Join(command, " "), user, workdir),
		cr.connect(),
		cr.find(),
		cr.exec(command, env, user, workdir),
	).IfNot(common.Dryrun)
}

func (cr *containerReference) Remove() common.Executor {
	return common.NewPipelineExecutor(
		cr.connect(),
		cr.find(),
	).Finally(
		cr.remove(),
	).IfNot(common.Dryrun)
}

func (cr *containerReference) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	out := cr.input.Stdout
	err := cr.input.Stderr

	cr.input.Stdout = stdout
	cr.input.Stderr = stderr

	return out, err
}

type containerReference struct {
	cli        client.APIClient
	id         string
	input      *NewContainerInput
	pullPolicy string
	UID        int
	GID        int
	// attachDone is closed by the attach() streaming goroutine once it has
	// drained and flushed the container's output. wait() blocks on it so the
	// tail of the log lands before the step proceeds.
	attachDone chan struct{}
	LinuxContainerEnvironmentExtensions
}

func GetDockerClient(ctx context.Context) (cli client.APIClient, err error) {
	dockerHost := os.Getenv("DOCKER_HOST")

	if strings.HasPrefix(dockerHost, "ssh://") {
		var helper *connhelper.ConnectionHelper

		helper, err = connhelper.GetConnectionHelper(dockerHost)
		if err != nil {
			return nil, err
		}
		cli, err = client.New(
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
		)
	} else {
		cli, err = client.New(client.FromEnv)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to docker daemon: %w", err)
	}
	// Best-effort API version negotiation, matching the old client.NegotiateAPIVersion
	// behaviour. Ping failures here are non-fatal: the connection is exercised again on
	// the first real call, and the client falls back to the daemon's default API version.
	if _, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		common.Logger(ctx).Warnf("docker daemon ping during version negotiation failed, continuing: %v", err)
	}

	return cli, nil
}

func GetHostInfo(ctx context.Context) (info system.Info, err error) {
	var cli client.APIClient
	cli, err = GetDockerClient(ctx)
	if err != nil {
		return info, err
	}
	defer cli.Close()

	result, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return info, err
	}

	return result.Info, nil
}

// Arch fetches values from docker info and translates architecture to
// GitHub actions compatible runner.arch values
// https://github.com/github/docs/blob/main/data/reusables/actions/runner-arch-description.md
func RunnerArch(ctx context.Context) string {
	info, err := GetHostInfo(ctx)
	if err != nil {
		return ""
	}

	archMapper := map[string]string{
		"x86_64":  "X64",
		"amd64":   "X64",
		"386":     "X86",
		"aarch64": "ARM64",
		"arm64":   "ARM64",
	}
	if arch, ok := archMapper[info.Architecture]; ok {
		return arch
	}
	return info.Architecture
}

func (cr *containerReference) connect() common.Executor {
	return func(ctx context.Context) error {
		if cr.cli != nil {
			return nil
		}
		cli, err := GetDockerClient(ctx)
		if err != nil {
			return err
		}
		cr.cli = cli
		return nil
	}
}

func (cr *containerReference) Close() common.Executor {
	return func(ctx context.Context) error {
		if cr.cli != nil {
			err := cr.cli.Close()
			cr.cli = nil
			if err != nil {
				return fmt.Errorf("failed to close client: %w", err)
			}
		}
		return nil
	}
}

// missingContainerError is the shared "container X does not exist" error used by ops that
// need a live cr.id, wrapping ErrContainerNotFound so a caller can tell it from a failing daemon.
func (cr *containerReference) missingContainerError(format string, args ...any) error {
	return fmt.Errorf("container %q %w; cannot "+format, append([]any{cr.input.Name, ErrContainerNotFound}, args...)...)
}

func (cr *containerReference) find() common.Executor {
	return func(ctx context.Context) error {
		if cr.id != "" {
			// Validate cached id; clear only on definitive NotFound so a
			// transient daemon error doesn't abort cleanup pipelines.
			_, err := cr.cli.ContainerInspect(ctx, cr.id, client.ContainerInspectOptions{})
			if !cerrdefs.IsNotFound(err) {
				return nil
			}
			cr.id = ""
		}
		containers, err := cr.cli.ContainerList(ctx, client.ContainerListOptions{
			All: true,
		})
		if err != nil {
			return fmt.Errorf("failed to list containers: %w", err)
		}

		for _, c := range containers.Items {
			for _, name := range c.Names {
				if name[1:] == cr.input.Name {
					cr.id = c.ID
					return nil
				}
			}
		}

		return nil
	}
}

func (cr *containerReference) remove() common.Executor {
	return func(ctx context.Context) error {
		idOrName := cr.id
		if idOrName == "" && cr.input != nil {
			idOrName = cr.input.Name
		}
		if idOrName == "" {
			return nil
		}

		logger := common.Logger(ctx)
		// Kill first so removal never waits out a daemon's stop timeout: Docker kills outright
		// on a forced remove, Podman sends SIGTERM and waits. Only worth it for a container
		// this started, and removal can still deal with one it could not kill.
		if cr.id != "" {
			_, err := cr.cli.ContainerKill(ctx, cr.id, client.ContainerKillOptions{Signal: "SIGKILL"})
			if err != nil && !cerrdefs.IsConflict(err) && !cerrdefs.IsNotFound(err) {
				logger.Debugf("Container %s could not be killed: %v", cr.id, err)
			}
		}
		_, err := cr.cli.ContainerRemove(ctx, idOrName, client.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		})
		if cerrdefs.IsConflict(err) {
			err = cr.waitForRemoval(ctx, idOrName)
		}
		if err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("failed to remove container %s: %w", idOrName, err)
		}

		logger.Debugf("Removed container: %v", idOrName)
		cr.id = ""
		return nil
	}
}

func (cr *containerReference) waitForRemoval(ctx context.Context, idOrName string) error {
	// per container, against the one minute the post-job executor allows for the whole
	// cleanup, so a job with several services can spend most of that budget here
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	waitResult := cr.cli.ContainerWait(ctx, idOrName, client.ContainerWaitOptions{
		Condition: container.WaitConditionRemoved,
	})
	select {
	case result := <-waitResult.Result:
		if result.Error != nil {
			return errors.New(result.Error.Message)
		}
		return nil
	case err := <-waitResult.Error:
		return err
	}
}

// allOptions puts the runner's options first, so a flag both sources set ends up the workflow's.
func (input *NewContainerInput) allOptions() string {
	return strings.TrimSpace(input.RunnerOptions + " " + input.WorkflowOptions)
}

func (cr *containerReference) mergeContainerConfigs(ctx context.Context, config *container.Config, hostConfig *container.HostConfig) (*container.Config, *container.HostConfig, error) {
	logger := common.Logger(ctx)
	options := cr.input.allOptions()

	if options == "" {
		return config, hostConfig, nil
	}

	// For Gitea, checked here because the parse below is what would read those files
	if err := rejectHostReadingOptions(cr.input.WorkflowOptions); err != nil {
		return nil, nil, err
	}

	// parse configuration from CLI container.options
	flags, copts, cf, err := parseContainerOptions(options)
	if err != nil {
		return nil, nil, err
	}

	if err := cf.validate(); err != nil {
		return nil, nil, fmt.Errorf("cannot process container options: '%s': '%w'", options, err)
	}

	// FIXME: If everything is fine after gitea/act v0.260.0, remove the following comment.
	// In the old fork version, the code is
	// if len(copts.netMode.Value()) == 0 {
	// 	if err = copts.netMode.Set("host"); err != nil {
	// 		return nil, nil, fmt.Errorf("cannot parse networkmode=host. This is an internal error and should not happen: '%w'", err)
	// 	}
	// }
	// And it has been commented with:
	//   If a service container's network is set to `host`, the container will not be able to
	//   connect to the specified network created for the job container and the service containers.
	//   So comment out the following code.
	// Not the if it's necessary to comment it in the new version,
	// since it's cr.input.NetworkMode now.

	if len(copts.netMode.Value()) == 0 {
		if err = copts.netMode.Set(cr.input.NetworkMode); err != nil {
			return nil, nil, fmt.Errorf("cannot parse networkmode=%s. This is an internal error and should not happen: '%w'", cr.input.NetworkMode, err)
		}
	}

	// If the `privileged` config has been disabled, `copts.privileged` need to be forced to false,
	// even if the user specifies `--privileged` in the options string.
	if !hostConfig.Privileged {
		copts.privileged = false
	}

	containerConfig, err := parse(flags, copts, runtime.GOOS)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot process container options: '%s': '%w'", options, err)
	}
	// workflow aliases join the runner's own, deduped so a re-create cannot grow the input
	for _, endpoint := range containerConfig.NetworkingConfig.EndpointsConfig {
		for _, alias := range endpoint.Aliases {
			if !slices.Contains(cr.input.NetworkAliases, alias) {
				cr.input.NetworkAliases = append(cr.input.NetworkAliases, alias)
			}
		}
	}

	// For Gitea, forcing --privileged off is not enough, other options reach the host too
	if !hostConfig.Privileged {
		trusted, err := parseOptionsHostConfig(cr.input.RunnerOptions)
		if err != nil {
			return nil, nil, err
		}
		sanitizeOptionsHostConfig(logger, containerConfig.HostConfig, trusted)
	}

	logger.Debugf("Custom container.Config from options ==> %+v", containerConfig.Config)

	err = mergo.Merge(config, containerConfig.Config, mergo.WithOverride, mergo.WithAppendSlice)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot merge container.Config options: '%s': '%w'", options, err)
	}
	logger.Debugf("Merged container.Config ==> %+v", config)

	logger.Debugf("Custom container.HostConfig from options ==> %+v", containerConfig.HostConfig)

	overlayVolumes(hostConfig, containerConfig.HostConfig)
	binds := hostConfig.Binds
	mounts := hostConfig.Mounts
	networkMode := hostConfig.NetworkMode
	err = mergo.Merge(hostConfig, containerConfig.HostConfig, mergo.WithOverride)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot merge container.HostConfig options: '%s': '%w'", options, err)
	}
	hostConfig.Binds = binds
	hostConfig.Mounts = mounts
	if cf.name != "" {
		logger.Warn("--name in the options will be ignored.")
	}
	// the runner's own network mode was put into copts above, so ask the flags instead
	if flags.Changed("network") || flags.Changed("net") {
		logger.Warn("--network and --net in the options will be ignored.")
	}
	hostConfig.NetworkMode = networkMode
	logger.Debugf("Merged container.HostConfig ==> %+v", hostConfig)

	return config, hostConfig, nil
}

func (cr *containerReference) create(capAdd, capDrop []string) common.Executor {
	return func(ctx context.Context) error {
		if cr.id != "" {
			return nil
		}
		logger := common.Logger(ctx)
		input := cr.input
		exposedPorts, err := convertPortSet(input.ExposedPorts)
		if err != nil {
			return err
		}
		portBindings, err := convertPortMap(input.PortBindings)
		if err != nil {
			return err
		}

		config := &container.Config{
			Image:        input.Image,
			WorkingDir:   input.WorkingDir,
			Env:          input.Env,
			ExposedPorts: exposedPorts,
			Tty:          input.AllocatePTY,
		}
		// For Gitea, reduce log noise
		// logger.Debugf("Common container.Config ==> %+v", config)

		if len(input.Cmd) != 0 {
			config.Cmd = input.Cmd
		}

		if len(input.Entrypoint) != 0 {
			config.Entrypoint = input.Entrypoint
		}

		mounts := make([]mount.Mount, 0)
		for mountSource, mountTarget := range input.Mounts {
			mounts = append(mounts, mount.Mount{
				Type:   mount.TypeVolume,
				Source: mountSource,
				Target: mountTarget,
			})
		}

		var platSpecs *specs.Platform
		if cr.input.Platform != "" {
			// Dropping the platform silently would build for the host arch.
			supported, err := supportsContainerImagePlatform(ctx, cr.cli)
			if err != nil {
				return err
			}
			if supported {
				if platSpecs, err = parsePlatform(cr.input.Platform); err != nil {
					return err
				}
			}
		}

		hostConfig := &container.HostConfig{
			CapAdd:       capAdd,
			CapDrop:      capDrop,
			Binds:        input.Binds,
			Mounts:       mounts,
			NetworkMode:  container.NetworkMode(input.NetworkMode),
			Privileged:   input.Privileged,
			UsernsMode:   container.UsernsMode(input.UsernsMode),
			PortBindings: portBindings,
			AutoRemove:   input.AutoRemove,
		}
		// For Gitea, reduce log noise
		// logger.Debugf("Common container.HostConfig ==> %+v", hostConfig)

		config, hostConfig, err = cr.mergeContainerConfigs(ctx, config, hostConfig)
		if err != nil {
			return err
		}

		// For Gitea
		config, hostConfig = cr.sanitizeConfig(ctx, config, hostConfig)

		var networkingConfig *network.NetworkingConfig
		// For Gitea, reduce log noise
		// logger.Debugf("input.NetworkAliases ==> %v", input.NetworkAliases)
		n := hostConfig.NetworkMode
		// IsUserDefined and IsHost are broken on windows
		if n.IsUserDefined() && n != "host" && len(input.NetworkAliases) > 0 {
			endpointConfig := &network.EndpointSettings{
				Aliases: input.NetworkAliases,
			}
			networkingConfig = &network.NetworkingConfig{
				EndpointsConfig: map[string]*network.EndpointSettings{
					input.NetworkMode: endpointConfig,
				},
			}
		}

		resp, err := cr.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:           config,
			HostConfig:       hostConfig,
			NetworkingConfig: networkingConfig,
			Platform:         platSpecs,
			Name:             input.Name,
		})
		if err != nil {
			return fmt.Errorf("failed to create container: '%w'", err)
		}

		logger.Debugf("Created container name=%s id=%v from image %v (platform: %s)", input.Name, resp.ID, input.Image, input.Platform)
		logger.Debugf("ENV ==> %v", input.Env)

		cr.id = resp.ID
		return nil
	}
}

func (cr *containerReference) extractFromImageEnv(env *map[string]string) common.Executor {
	envMap := *env
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)

		inspect, err := cr.cli.ImageInspect(ctx, cr.input.Image)
		if err != nil {
			logger.Error(err)
			return fmt.Errorf("inspect image: %w", err)
		}

		if inspect.Config == nil {
			return nil
		}

		imageEnv, err := godotenv.Unmarshal(strings.Join(inspect.Config.Env, "\n"))
		if err != nil {
			logger.Error(err)
			return fmt.Errorf("unmarshal image env: %w", err)
		}

		for k, v := range imageEnv {
			if k == "PATH" {
				if envMap[k] == "" {
					envMap[k] = v
				} else {
					envMap[k] += `:` + v
				}
			} else if envMap[k] == "" {
				envMap[k] = v
			}
		}

		env = &envMap
		return nil
	}
}

func (cr *containerReference) exec(cmd []string, env map[string]string, user, workdir string) common.Executor {
	return func(ctx context.Context) error {
		if cr.id == "" {
			return cr.missingContainerError("exec %v", cmd)
		}
		logger := common.Logger(ctx)
		// Fix slashes when running on Windows
		if runtime.GOOS == "windows" {
			var newCmd []string
			for _, v := range cmd {
				newCmd = append(newCmd, strings.ReplaceAll(v, `\`, `/`))
			}
			cmd = newCmd
		}

		logger.Debugf("Exec command '%s'", cmd)
		isTerminal := cr.input.AllocatePTY
		envList := make([]string, 0)
		for k, v := range env {
			envList = append(envList, fmt.Sprintf("%s=%s", k, v))
		}

		var wd string
		if workdir != "" {
			if strings.HasPrefix(workdir, "/") {
				wd = workdir
			} else {
				wd = fmt.Sprintf("%s/%s", cr.input.WorkingDir, workdir)
			}
		} else {
			wd = cr.input.WorkingDir
		}
		logger.Debugf("Working directory '%s'", wd)

		idResp, err := cr.cli.ExecCreate(ctx, cr.id, client.ExecCreateOptions{
			User:         user,
			Cmd:          cmd,
			WorkingDir:   wd,
			Env:          envList,
			TTY:          isTerminal,
			AttachStderr: true,
			AttachStdout: true,
		})
		if err != nil {
			return fmt.Errorf("failed to create exec: %w", err)
		}

		resp, err := cr.cli.ExecAttach(ctx, idResp.ID, client.ExecAttachOptions{
			TTY: isTerminal,
		})
		if err != nil {
			return fmt.Errorf("failed to attach to exec: %w", err)
		}
		defer resp.Close()

		err = cr.waitForCommand(ctx, resp.HijackedResponse, idResp, user, workdir)
		if err != nil {
			return err
		}

		inspectResp, err := cr.cli.ExecInspect(ctx, idResp.ID, client.ExecInspectOptions{})
		if err != nil {
			return fmt.Errorf("failed to inspect exec: %w", err)
		}

		if inspectResp.ExitCode == 0 {
			return nil
		}
		return ExitCodeError(inspectResp.ExitCode)
	}
}

func (cr *containerReference) tryReadID(opt string, cbk func(id int)) common.Executor {
	return func(ctx context.Context) error {
		idResp, err := cr.cli.ExecCreate(ctx, cr.id, client.ExecCreateOptions{
			Cmd:          []string{"id", opt},
			AttachStdout: true,
			AttachStderr: true,
		})
		if err != nil {
			return nil
		}

		resp, err := cr.cli.ExecAttach(ctx, idResp.ID, client.ExecAttachOptions{})
		if err != nil {
			return nil
		}
		defer resp.Close()

		sid, err := resp.Reader.ReadString('\n')
		if err != nil {
			return nil
		}
		exp := regexp.MustCompile(`\d+\n`)
		found := exp.FindString(sid)
		id, err := strconv.ParseInt(strings.TrimSpace(found), 10, 32)
		if err != nil {
			return nil
		}
		cbk(int(id))

		return nil
	}
}

func (cr *containerReference) tryReadUID() common.Executor {
	return cr.tryReadID("-u", func(id int) { cr.UID = id })
}

func (cr *containerReference) tryReadGID() common.Executor {
	return cr.tryReadID("-g", func(id int) { cr.GID = id })
}

func (cr *containerReference) waitForCommand(ctx context.Context, resp client.HijackedResponse, _ client.ExecCreateResult, _, _ string) error {
	logger := common.Logger(ctx)

	// Buffered so the copy goroutine never blocks on send if the grace-period
	// drain below times out and no one is left to receive.
	cmdResponse := make(chan error, 1)

	go func() {
		cmdResponse <- cr.copyOutput(resp.Reader)
	}()

	select {
	case <-ctx.Done():
		// send ctrl + c
		_, err := resp.Conn.Write([]byte{3})
		if err != nil {
			logger.Warnf("Failed to send CTRL+C: %+s", err)
		}

		// Give the copy goroutine a brief grace period to drain output already
		// produced by the command before we return, so cancellation does not
		// truncate the tail of the log. The goroutine exits once the hijacked
		// stream is closed by resp.Close() in the caller's defer.
		select {
		case <-cmdResponse:
		case <-time.After(drainGracePeriod):
			logger.Warn("Timed out draining command output after cancellation")
		}

		// we return the context canceled error to prevent other steps
		// from executing
		return ctx.Err()
	case err := <-cmdResponse:
		if err != nil {
			logger.Error(err)
		}

		return nil
	}
}

func (cr *containerReference) copyDir(dstPath, srcPath string, useGitIgnore, skipGitDir bool) common.Executor {
	return func(ctx context.Context) error {
		if cr.id == "" {
			return cr.missingContainerError("copy directory to %s", dstPath)
		}
		logger := common.Logger(ctx)
		tarFile, err := os.CreateTemp("", "act")
		if err != nil {
			return err
		}
		logger.Debugf("Writing tarball %s from %s", tarFile.Name(), srcPath)
		defer func(tarFile *os.File) {
			name := tarFile.Name()
			err := tarFile.Close()
			if !errors.Is(err, os.ErrClosed) {
				logger.Error(err)
			}
			err = os.Remove(name)
			if err != nil {
				logger.Error(err)
			}
		}(tarFile)
		tw := tar.NewWriter(tarFile)

		srcPrefix := filepath.Dir(srcPath)
		if !strings.HasSuffix(srcPrefix, string(filepath.Separator)) {
			srcPrefix += string(filepath.Separator)
		}
		logger.Debugf("Stripping prefix:%s src:%s", srcPrefix, srcPath)

		var ignorer gitignore.Matcher
		if useGitIgnore {
			ps, err := gitignore.ReadPatterns(polyfill.New(osfs.New(srcPath)), nil)
			if err != nil {
				logger.Debugf("Error loading .gitignore: %v", err)
			}

			ignorer = gitignore.NewMatcher(ps)
		}

		fc := &filecollector.FileCollector{
			Ignorer:    ignorer,
			SrcPath:    srcPath,
			SrcPrefix:  srcPrefix,
			SkipGitDir: skipGitDir,
			Handler: &filecollector.TarCollector{
				TarWriter: tw,
				UID:       cr.UID,
				GID:       cr.GID,
				DstDir:    dstPath[1:],
			},
		}

		err = filepath.Walk(srcPath, fc.CollectFiles(ctx, []string{}))
		if err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}

		logger.Debugf("Extracting content from '%s' to '%s'", tarFile.Name(), dstPath)
		_, err = tarFile.Seek(0, 0)
		if err != nil {
			return fmt.Errorf("failed to seek tar archive: %w", err)
		}
		_, err = cr.cli.CopyToContainer(ctx, cr.id, client.CopyToContainerOptions{
			DestinationPath: "/",
			Content:         tarFile,
		})
		if err != nil {
			return fmt.Errorf("failed to copy content to container: %w", err)
		}
		return nil
	}
}

func (cr *containerReference) copyContent(dstPath string, files ...*FileEntry) common.Executor {
	return func(ctx context.Context) error {
		if cr.id == "" {
			return cr.missingContainerError("copy to %s", dstPath)
		}
		logger := common.Logger(ctx)
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, file := range files {
			logger.Debugf("Writing entry to tarball %s len:%d", file.Name, len(file.Body))
			hdr := &tar.Header{
				Name: file.Name,
				Mode: file.Mode,
				Size: int64(len(file.Body)),
				Uid:  cr.UID,
				Gid:  cr.GID,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write([]byte(file.Body)); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}

		logger.Debugf("Extracting content to '%s'", dstPath)
		_, err := cr.cli.CopyToContainer(ctx, cr.id, client.CopyToContainerOptions{
			DestinationPath: dstPath,
			Content:         &buf,
		})
		if err != nil {
			return fmt.Errorf("failed to copy content to container: %w", err)
		}
		return nil
	}
}

func (cr *containerReference) attach() common.Executor {
	return func(ctx context.Context) error {
		out, err := cr.cli.ContainerAttach(ctx, cr.id, client.ContainerAttachOptions{
			Stream: true,
			Stdout: true,
			Stderr: true,
		})
		if err != nil {
			return fmt.Errorf("failed to attach to container: %w", err)
		}
		done := make(chan struct{})
		cr.attachDone = done
		go func() {
			defer close(done)
			if copyErr := cr.copyOutput(out.Reader); copyErr != nil {
				common.Logger(ctx).Error(copyErr)
			}
		}()
		return nil
	}
}

func (cr *containerReference) start() common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		logger.Debugf("Starting container: %v", cr.id)

		if _, err := cr.cli.ContainerStart(ctx, cr.id, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("failed to start container: %w", err)
		}

		logger.Debugf("Started container: %v", cr.id)
		return nil
	}
}

func (cr *containerReference) wait() common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		waitResult := cr.cli.ContainerWait(ctx, cr.id, client.ContainerWaitOptions{
			Condition: container.WaitConditionNotRunning,
		})
		var statusCode int64
		select {
		case err := <-waitResult.Error:
			if err != nil {
				return fmt.Errorf("failed to wait for container: %w", err)
			}
		case status := <-waitResult.Result:
			statusCode = status.StatusCode
		}

		logger.Debugf("Return status: %v", statusCode)

		// The container has exited; wait for the attach() streaming goroutine to
		// finish draining and flushing its output before returning, so the tail
		// of the log is not lost. Bounded so a stuck stream cannot hang the step.
		if cr.attachDone != nil {
			select {
			case <-cr.attachDone:
			case <-time.After(drainGracePeriod):
				logger.Warn("Timed out draining container output")
			}
			cr.attachDone = nil
		}

		if statusCode == 0 {
			return nil
		}

		return ExitCodeError(statusCode)
	}
}

// For Gitea
// sanitizeOptionsHostConfig takes back everything a workflow could escape the container with,
// setting each field to trusted, which is what the runner's own options parse to on their own.
// Only for unprivileged mode, since privileged mode grants host access anyway.
func sanitizeOptionsHostConfig(logger logrus.FieldLogger, hostConfig, trusted *container.HostConfig) {
	resetOption(logger, "--pid", &hostConfig.PidMode, trusted.PidMode)
	resetOption(logger, "--ipc", &hostConfig.IpcMode, trusted.IpcMode)
	resetOption(logger, "--uts", &hostConfig.UTSMode, trusted.UTSMode)
	resetOption(logger, "--cgroupns", &hostConfig.CgroupnsMode, trusted.CgroupnsMode)
	resetOption(logger, "--userns", &hostConfig.UsernsMode, trusted.UsernsMode) // --userns=host would undo the remapping the runner asked for
	resetOption(logger, "--cap-add", &hostConfig.CapAdd, trusted.CapAdd)
	resetOption(logger, "--security-opt", &hostConfig.SecurityOpt, trusted.SecurityOpt)
	resetOption(logger, "--device", &hostConfig.Devices, trusted.Devices)
	resetOption(logger, "--device-cgroup-rule", &hostConfig.DeviceCgroupRules, trusted.DeviceCgroupRules)
	resetOption(logger, "--gpus", &hostConfig.DeviceRequests, trusted.DeviceRequests)
	resetOption(logger, "--volumes-from", &hostConfig.VolumesFrom, trusted.VolumesFrom)
	resetOption(logger, "--runtime", &hostConfig.Runtime, trusted.Runtime)
	resetOption(logger, "--cgroup-parent", &hostConfig.CgroupParent, trusted.CgroupParent)
	resetOption(logger, "--sysctl", &hostConfig.Sysctls, trusted.Sysctls)
	resetOption(logger, "--isolation", &hostConfig.Isolation, trusted.Isolation) // windows: process isolation drops the hyper-v boundary
	resetOption(logger, "--volume-driver", &hostConfig.VolumeDriver, trusted.VolumeDriver)
	// systempaths=unconfined lands in these two rather than in SecurityOpt
	resetOption(logger, "--security-opt", &hostConfig.MaskedPaths, trusted.MaskedPaths)
	resetOption(logger, "--security-opt", &hostConfig.ReadonlyPaths, trusted.ReadonlyPaths)

	// a driver mounts what it likes, e.g. local with device= binds any host path, which
	// valid_volumes never gets to see
	hostConfig.Mounts = slices.DeleteFunc(hostConfig.Mounts, func(mt mount.Mount) bool {
		if mt.VolumeOptions == nil || mt.VolumeOptions.DriverConfig == nil ||
			slices.ContainsFunc(trusted.Mounts, func(t mount.Mount) bool { return reflect.DeepEqual(t, mt) }) {
			return false
		}
		logger.Warnf("volume driver of %q in the workflow is not allowed when privileged mode is disabled and will be ignored", mt.Source)
		return true
	})
}

// resetOption puts a field back to the runner's own value. It compares the values rather than
// the flags, so a field that more than one option feeds cannot slip through.
func resetOption[T any](logger logrus.FieldLogger, option string, field *T, trusted T) {
	if reflect.DeepEqual(*field, trusted) {
		return
	}
	logger.Warnf("container option %q in the workflow is not allowed when privileged mode is disabled and will be ignored", option)
	*field = trusted
}

// parseOptionsHostConfig parses one options string on its own, to see what it alone asks for.
// Even "" goes through the parser, or its empty slices and maps would differ from a real parse.
func parseOptionsHostConfig(options string) (*container.HostConfig, error) {
	flags, copts, _, err := parseContainerOptions(options)
	if err != nil {
		return nil, err
	}
	containerConfig, err := parse(flags, copts, runtime.GOOS)
	if err != nil {
		return nil, fmt.Errorf("cannot process container options: '%s': '%w'", options, err)
	}
	return containerConfig.HostConfig, nil
}

// For Gitea
// sanitizeConfig remove the invalid configurations from `config` and `hostConfig`
func (cr *containerReference) sanitizeConfig(ctx context.Context, config *container.Config, hostConfig *container.HostConfig) (*container.Config, *container.HostConfig) {
	logger := common.Logger(ctx)

	if len(cr.input.ValidVolumes) > 0 {
		matcher := newValidVolumeMatcher(ctx, cr.input.ValidVolumes)
		// sanitize binds
		sanitizedBinds := make([]string, 0, len(hostConfig.Binds))
		for _, bind := range hostConfig.Binds {
			parsed, err := loader.ParseVolume(bind)
			if err != nil {
				logger.Warnf("parse volume [%s] error: %v", bind, err)
				continue
			}
			if parsed.Source == "" {
				// anonymous volume
				sanitizedBinds = append(sanitizedBinds, bind)
				continue
			}
			if matcher.isValid(parsed.Source, mount.Type(parsed.Type)) {
				sanitizedBinds = append(sanitizedBinds, bind)
			} else {
				logger.Warnf("[%s] is not a valid volume, will be ignored", parsed.Source)
			}
		}
		hostConfig.Binds = sanitizedBinds
		// sanitize mounts
		sanitizedMounts := make([]mount.Mount, 0, len(hostConfig.Mounts))
		for _, mt := range hostConfig.Mounts {
			if matcher.isValid(mt.Source, mt.Type) {
				sanitizedMounts = append(sanitizedMounts, mt)
			} else {
				logger.Warnf("[%s] is not a valid volume, will be ignored", mt.Source)
			}
		}
		hostConfig.Mounts = sanitizedMounts
	} else {
		for _, bind := range hostConfig.Binds {
			logger.Warnf("[%s] is not a valid volume, will be ignored", bind)
		}
		for _, mt := range hostConfig.Mounts {
			logger.Warnf("[%s] is not a valid volume, will be ignored", mt.Source)
		}
		hostConfig.Binds = []string{}
		hostConfig.Mounts = []mount.Mount{}
	}

	return config, hostConfig
}

// bindTarget returns the container path a bind mounts onto, empty if it cannot be parsed.
func bindTarget(bind string) string {
	parsed, err := loader.ParseVolume(bind)
	if err != nil {
		return ""
	}
	return parsed.Target
}

// overlayVolumes appends src's volumes to dst, dropping the dst ones they mount over. Docker
// rejects two mounts on one target, so the volumes declared last have to win.
func overlayVolumes(dst, src *container.HostConfig) {
	claimed := map[string]bool{}
	for _, bind := range src.Binds {
		if target := bindTarget(bind); target != "" {
			claimed[target] = true
		}
	}
	for _, mt := range src.Mounts {
		claimed[mt.Target] = true
	}

	dst.Binds = append(slices.DeleteFunc(slices.Clone(dst.Binds),
		func(bind string) bool { return claimed[bindTarget(bind)] }), src.Binds...)
	dst.Mounts = append(slices.DeleteFunc(slices.Clone(dst.Mounts),
		func(mt mount.Mount) bool { return claimed[mt.Target] }), src.Mounts...)
}

type validVolumeMatcher struct {
	allowAll bool
	named    []string
	host     []string
}

func newValidVolumeMatcher(ctx context.Context, validVolumes []string) validVolumeMatcher {
	logger := common.Logger(ctx)
	ret := validVolumeMatcher{
		named: make([]string, 0, len(validVolumes)),
		host:  make([]string, 0, len(validVolumes)),
	}

	for _, v := range validVolumes {
		if v == "**" {
			ret.allowAll = true
			continue
		}
		if !isHostVolumePattern(v) {
			if doublestar.ValidatePattern(v) {
				ret.named = append(ret.named, v)
			} else {
				logger.Errorf("invalid volume pattern %s", v)
			}
			continue
		}
		normalized, err := normalizeHostVolumePath(v)
		if err != nil {
			logger.Errorf("normalize volume pattern %s error: %v", v, err)
			continue
		}
		if doublestar.ValidatePathPattern(normalized) {
			ret.host = append(ret.host, normalized)
		} else {
			logger.Errorf("invalid volume pattern %s", normalized)
		}
	}

	return ret
}

func (m validVolumeMatcher) isValid(source string, sourceType mount.Type) bool {
	if m.allowAll {
		return true
	}
	if isHostVolumeSource(source, sourceType) {
		normalized, err := normalizeHostVolumePath(source)
		if err != nil {
			return false
		}
		for _, pattern := range m.host {
			if doublestar.PathMatchUnvalidated(pattern, normalized) {
				return true
			}
		}
		return false
	}
	for _, pattern := range m.named {
		if doublestar.MatchUnvalidated(pattern, source) {
			return true
		}
	}
	return false
}

func isHostVolumePattern(pattern string) bool {
	return filepath.IsAbs(pattern) ||
		strings.HasPrefix(pattern, "."+string(filepath.Separator)) ||
		strings.HasPrefix(pattern, ".."+string(filepath.Separator)) ||
		strings.Contains(pattern, "/") ||
		strings.Contains(pattern, `\`)
}

func isHostVolumeSource(source string, sourceType mount.Type) bool {
	if sourceType == mount.TypeBind {
		return true
	}
	if sourceType == mount.TypeVolume {
		return false
	}
	return isHostVolumePattern(source)
}

func normalizeHostVolumePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return evalSymlinksExistingPrefix(abs)
}

func evalSymlinksExistingPrefix(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	current := path
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for _, name := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, name)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path), nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
