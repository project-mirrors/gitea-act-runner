// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDocker(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	client, err := GetDockerClient(ctx)
	require.NoError(t, err)
	defer client.Close()

	dockerBuild := NewDockerBuildExecutor(NewDockerBuildExecutorInput{
		ContextDir: "testdata",
		ImageTag:   "envmergetest",
	})

	err = dockerBuild(ctx)
	assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act

	cr := &containerReference{
		cli: client,
		input: &NewContainerInput{
			Image: "envmergetest",
		},
	}
	env := map[string]string{
		"PATH":         "/usr/local/bin:/usr/bin:/usr/sbin:/bin:/sbin",
		"RANDOM_VAR":   "WITH_VALUE",
		"ANOTHER_VAR":  "",
		"CONFLICT_VAR": "I_EXIST_IN_MULTIPLE_PLACES",
	}

	envExecutor := cr.extractFromImageEnv(&env)
	err = envExecutor(ctx)
	assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act
	assert.Equal(t, map[string]string{
		"PATH":            "/usr/local/bin:/usr/bin:/usr/sbin:/bin:/sbin:/this/path/does/not/exists/anywhere:/this/either",
		"RANDOM_VAR":      "WITH_VALUE",
		"ANOTHER_VAR":     "",
		"SOME_RANDOM_VAR": "",
		"ANOTHER_ONE":     "BUT_I_HAVE_VALUE",
		"CONFLICT_VAR":    "I_EXIST_IN_MULTIPLE_PLACES",
	}, env)
}

type mockDockerClient struct {
	mobyclient.APIClient
	mock.Mock
}

func (m *mockDockerClient) ServerVersion(ctx context.Context, opts mobyclient.ServerVersionOptions) (mobyclient.ServerVersionResult, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(mobyclient.ServerVersionResult), args.Error(1)
}

func (m *mockDockerClient) ExecCreate(ctx context.Context, id string, opts mobyclient.ExecCreateOptions) (mobyclient.ExecCreateResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.ExecCreateResult), args.Error(1)
}

func (m *mockDockerClient) ExecAttach(ctx context.Context, id string, opts mobyclient.ExecAttachOptions) (mobyclient.ExecAttachResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.ExecAttachResult), args.Error(1)
}

func (m *mockDockerClient) ExecInspect(ctx context.Context, execID string, opts mobyclient.ExecInspectOptions) (mobyclient.ExecInspectResult, error) {
	args := m.Called(ctx, execID, opts)
	return args.Get(0).(mobyclient.ExecInspectResult), args.Error(1)
}

func (m *mockDockerClient) ContainerAttach(ctx context.Context, containerID string, opts mobyclient.ContainerAttachOptions) (mobyclient.ContainerAttachResult, error) {
	args := m.Called(ctx, containerID, opts)
	return args.Get(0).(mobyclient.ContainerAttachResult), args.Error(1)
}

func (m *mockDockerClient) ContainerWait(ctx context.Context, containerID string, opts mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult {
	args := m.Called(ctx, containerID, opts)
	return args.Get(0).(mobyclient.ContainerWaitResult)
}

func (m *mockDockerClient) CopyToContainer(ctx context.Context, id string, options mobyclient.CopyToContainerOptions) (mobyclient.CopyToContainerResult, error) {
	args := m.Called(ctx, id, options)
	return args.Get(0).(mobyclient.CopyToContainerResult), args.Error(1)
}

func (m *mockDockerClient) ContainerInspect(ctx context.Context, id string, opts mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.ContainerInspectResult), args.Error(1)
}

func (m *mockDockerClient) ContainerList(ctx context.Context, opts mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(mobyclient.ContainerListResult), args.Error(1)
}

func (m *mockDockerClient) ContainerRemove(ctx context.Context, id string, opts mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.ContainerRemoveResult), args.Error(1)
}

func (m *mockDockerClient) ContainerKill(ctx context.Context, id string, opts mobyclient.ContainerKillOptions) (mobyclient.ContainerKillResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.ContainerKillResult), args.Error(1)
}

func (m *mockDockerClient) NetworkList(ctx context.Context, opts mobyclient.NetworkListOptions) (mobyclient.NetworkListResult, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(mobyclient.NetworkListResult), args.Error(1)
}

func (m *mockDockerClient) NetworkInspect(ctx context.Context, id string, opts mobyclient.NetworkInspectOptions) (mobyclient.NetworkInspectResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.NetworkInspectResult), args.Error(1)
}

func (m *mockDockerClient) NetworkRemove(ctx context.Context, id string, opts mobyclient.NetworkRemoveOptions) (mobyclient.NetworkRemoveResult, error) {
	args := m.Called(ctx, id, opts)
	return args.Get(0).(mobyclient.NetworkRemoveResult), args.Error(1)
}

type interruptReader struct {
	started     chan struct{}
	interrupted chan struct{}
	stopped     chan struct{}
}

func (r *interruptReader) Read(_ []byte) (int, error) {
	close(r.started)
	<-r.interrupted
	close(r.stopped)
	return 0, io.EOF
}

type mockConn struct {
	net.Conn
	mock.Mock
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	args := m.Called(b)
	return args.Int(0), args.Error(1)
}

func (m *mockConn) Close() (err error) {
	return nil
}

func TestDockerExecAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	reader := &interruptReader{started: make(chan struct{}), interrupted: make(chan struct{}), stopped: make(chan struct{})}
	conn := &mockConn{}
	conn.On("Write", []byte{3}).
		Run(func(mock.Arguments) { close(reader.interrupted) }).
		Return(1, nil)

	client := &mockDockerClient{}
	client.On("ExecCreate", ctx, "123", mock.AnythingOfType("client.ExecCreateOptions")).Return(mobyclient.ExecCreateResult{ID: "id"}, nil)
	client.On("ExecAttach", ctx, "id", mock.AnythingOfType("client.ExecAttachOptions")).Return(mobyclient.ExecAttachResult{
		Conn:   conn,
		Reader: bufio.NewReader(reader),
	}, nil)

	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &NewContainerInput{
			Image: "image",
		},
	}

	channel := make(chan error)

	go func() {
		channel <- cr.exec([]string{""}, map[string]string{}, "user", "workdir")(ctx)
	}()

	<-reader.started
	cancel()

	err := <-channel
	<-reader.stopped
	assert.ErrorIs(t, err, context.Canceled) //nolint:testifylint // pre-existing issue from nektos/act

	conn.AssertExpectations(t)
	client.AssertExpectations(t)
}

func TestDockerExecFailure(t *testing.T) {
	ctx := context.Background()

	conn := &mockConn{}

	client := &mockDockerClient{}
	client.On("ExecCreate", ctx, "123", mock.AnythingOfType("client.ExecCreateOptions")).Return(mobyclient.ExecCreateResult{ID: "id"}, nil)
	client.On("ExecAttach", ctx, "id", mock.AnythingOfType("client.ExecAttachOptions")).Return(mobyclient.ExecAttachResult{
		Conn:   conn,
		Reader: bufio.NewReader(strings.NewReader("output")),
	}, nil)
	client.On("ExecInspect", ctx, "id", mobyclient.ExecInspectOptions{}).Return(mobyclient.ExecInspectResult{
		ExitCode: 1,
	}, nil)

	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &NewContainerInput{
			Image: "image",
		},
	}

	err := cr.exec([]string{""}, map[string]string{}, "user", "workdir")(ctx)
	var exitErr ExitCodeError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, ExitCodeError(1), exitErr)
	assert.Equal(t, "Process completed with exit code 1.", err.Error())

	conn.AssertExpectations(t)
	client.AssertExpectations(t)
}

// stdcopyFrame wraps payload in a single Docker multiplexed-stream frame, the
// format StdCopy expects: an 8-byte header (stream type + 4-byte big-endian
// length) followed by the payload.
func stdcopyFrame(stream stdcopy.StdType, payload string) []byte {
	b := make([]byte, 8+len(payload))
	b[0] = byte(stream)
	binary.BigEndian.PutUint32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

// TestDockerAttachFlushesTrailingLine verifies that wait() blocks until the
// attach() streaming goroutine has drained and flushed the container's output,
// so a final line without a trailing newline is not lost.
func TestDockerAttachFlushesTrailingLine(t *testing.T) {
	ctx := context.Background()

	framed := bytes.NewBuffer(stdcopyFrame(stdcopy.Stdout, "line one\nlast line without newline"))

	var lines []string
	logWriter := common.NewLineWriter(func(s string) bool {
		lines = append(lines, s)
		return true
	})

	client := &mockDockerClient{}
	client.On("ContainerAttach", ctx, "123", mock.AnythingOfType("client.ContainerAttachOptions")).
		Return(mobyclient.ContainerAttachResult{
			Conn:   &mockConn{},
			Reader: bufio.NewReader(framed),
		}, nil)

	statusCh := make(chan container.WaitResponse, 1)
	statusCh <- container.WaitResponse{StatusCode: 0}
	errCh := make(chan error, 1)
	client.On("ContainerWait", ctx, "123", mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning}).
		Return(mobyclient.ContainerWaitResult{
			Result: (<-chan container.WaitResponse)(statusCh),
			Error:  (<-chan error)(errCh),
		})

	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &NewContainerInput{
			Image:  "image",
			Stdout: logWriter,
			Stderr: logWriter,
		},
	}

	require.NoError(t, cr.attach()(ctx))
	require.NoError(t, cr.wait()(ctx))

	// wait() must have blocked until the goroutine drained AND flushed; the
	// trailing, non-newline-terminated line must therefore be present. Reading
	// lines here is race-free because wait() synchronizes on attachDone, which
	// the goroutine closes after the final append.
	assert.Equal(t, []string{"line one\n", "last line without newline"}, lines)

	client.AssertExpectations(t)
}

func TestDockerWaitFailure(t *testing.T) {
	ctx := context.Background()

	statusCh := make(chan container.WaitResponse, 1)
	statusCh <- container.WaitResponse{StatusCode: 2}
	errCh := make(chan error, 1)

	client := &mockDockerClient{}
	client.On("ContainerWait", ctx, "123", mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning}).
		Return(mobyclient.ContainerWaitResult{
			Result: (<-chan container.WaitResponse)(statusCh),
			Error:  (<-chan error)(errCh),
		})

	cr := &containerReference{
		id:  "123",
		cli: client,
		input: &NewContainerInput{
			Image: "image",
		},
	}

	err := cr.wait()(ctx)
	var exitErr ExitCodeError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, ExitCodeError(2), exitErr)
	assert.Equal(t, "Process completed with exit code 2.", err.Error())

	client.AssertExpectations(t)
}

// A remove that raced the daemon's AutoRemove teardown is not a failure and must not
// be logged as one.
func TestRemoveIgnoresAutoRemoveRace(t *testing.T) {
	removeFailure := errors.New("driver failed to remove root filesystem")
	removeOpts := mobyclient.ContainerRemoveOptions{RemoveVolumes: true, Force: true}
	killOpts := mobyclient.ContainerKillOptions{Signal: "SIGKILL"}
	for _, tc := range []struct {
		name     string
		err      error
		wantWait bool
		waitErr  error
		wantErr  error
	}{
		{name: "removal in progress", err: cerrdefs.ErrConflict.WithMessage("removal of container abc is already in progress"), wantWait: true},
		{name: "wait canceled", err: cerrdefs.ErrConflict, wantWait: true, waitErr: context.Canceled, wantErr: context.Canceled},
		{name: "removed during wait", err: cerrdefs.ErrConflict, wantWait: true, waitErr: cerrdefs.ErrNotFound},
		{name: "already removed", err: cerrdefs.ErrNotFound.WithMessage("No such container: abc")},
		{name: "removed cleanly", err: nil},
		{name: "real failure", err: removeFailure, wantErr: removeFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, hook := test.NewNullLogger()
			ctx := common.WithLogger(context.Background(), logger)
			client := &mockDockerClient{}
			client.On("ContainerKill", ctx, "abc", killOpts).Return(mobyclient.ContainerKillResult{}, nil)
			client.On("ContainerRemove", ctx, "abc", removeOpts).Return(mobyclient.ContainerRemoveResult{}, tc.err)
			if tc.wantWait {
				removed := make(chan container.WaitResponse, 1)
				waitErrors := make(chan error, 1)
				if tc.waitErr == nil {
					removed <- container.WaitResponse{}
				} else {
					waitErrors <- tc.waitErr
				}
				client.On("ContainerWait", mock.Anything, "abc", mobyclient.ContainerWaitOptions{Condition: container.WaitConditionRemoved}).
					Return(mobyclient.ContainerWaitResult{Result: removed, Error: waitErrors})
			}
			cr := &containerReference{id: "abc", cli: client}

			require.ErrorIs(t, cr.remove()(ctx), tc.wantErr)
			if tc.wantErr != nil {
				assert.Equal(t, "abc", cr.id)
			} else {
				assert.Empty(t, cr.id)
			}
			assert.Empty(t, hook.AllEntries())
			client.AssertExpectations(t)
		})
	}
}

// A container whose id was never learned, because find() could not reach the daemon or
// create() lost its reply, must still be removed rather than leaking with its network. It
// was never started here, so it is not worth a kill of its own.
func TestRemoveWithoutIDUsesName(t *testing.T) {
	ctx := context.Background()
	client := &mockDockerClient{}
	client.On("ContainerRemove", ctx, "job-1", mobyclient.ContainerRemoveOptions{RemoveVolumes: true, Force: true}).
		Return(mobyclient.ContainerRemoveResult{}, nil)
	cr := &containerReference{cli: client, input: &NewContainerInput{Name: "job-1"}}

	require.NoError(t, cr.remove()(ctx))
	client.AssertExpectations(t)
}

// find() must drop a stale cached id so later Copy/Exec don't hit the
// daemon with a torn-down container.
func TestFindRevalidatesStaleID(t *testing.T) {
	ctx := context.Background()
	notFound := cerrdefs.ErrNotFound.WithMessage("No such container")
	boom := errors.New("daemon unreachable")
	newCR := func(id string) (*containerReference, *mockDockerClient) {
		client := &mockDockerClient{}
		return &containerReference{id: id, cli: client, input: &NewContainerInput{Name: "job-1"}}, client
	}
	listOpts := mobyclient.ContainerListOptions{All: true}
	inspectOpts := mobyclient.ContainerInspectOptions{}

	t.Run("stale id cleared, name lookup empty", func(t *testing.T) {
		cr, client := newCR("stale")
		client.On("ContainerInspect", ctx, "stale", inspectOpts).Return(mobyclient.ContainerInspectResult{}, notFound)
		client.On("ContainerList", ctx, listOpts).Return(mobyclient.ContainerListResult{}, nil)
		require.NoError(t, cr.find()(ctx))
		assert.Empty(t, cr.id)
		client.AssertExpectations(t)
	})

	t.Run("stale id cleared, name lookup repopulates", func(t *testing.T) {
		cr, client := newCR("stale")
		client.On("ContainerInspect", ctx, "stale", inspectOpts).Return(mobyclient.ContainerInspectResult{}, notFound)
		client.On("ContainerList", ctx, listOpts).Return(mobyclient.ContainerListResult{Items: []container.Summary{
			{ID: "other", Names: []string{"/somebody-else"}},
			{ID: "fresh", Names: []string{"/job-1"}},
		}}, nil)
		require.NoError(t, cr.find()(ctx))
		assert.Equal(t, "fresh", cr.id)
		client.AssertExpectations(t)
	})

	t.Run("live id kept", func(t *testing.T) {
		cr, client := newCR("live")
		client.On("ContainerInspect", ctx, "live", inspectOpts).Return(mobyclient.ContainerInspectResult{}, nil)
		require.NoError(t, cr.find()(ctx))
		assert.Equal(t, "live", cr.id)
		client.AssertExpectations(t)
	})

	t.Run("transient inspect error trusts cache", func(t *testing.T) {
		cr, client := newCR("live")
		client.On("ContainerInspect", ctx, "live", inspectOpts).Return(mobyclient.ContainerInspectResult{}, boom)
		require.NoError(t, cr.find()(ctx))
		assert.Equal(t, "live", cr.id)
		client.AssertExpectations(t)
	})

	t.Run("list error propagates", func(t *testing.T) {
		cr, client := newCR("")
		client.On("ContainerList", ctx, listOpts).Return(mobyclient.ContainerListResult{}, boom)
		require.ErrorIs(t, cr.find()(ctx), boom)
		client.AssertExpectations(t)
	})
}

// Every daemon entry point fails fast with a clear, container-named
// error when no live cr.id is known.
func TestRejectsMissingContainer(t *testing.T) {
	ctx := context.Background()
	client := &mockDockerClient{}
	client.On("ContainerList", ctx, mobyclient.ContainerListOptions{All: true}).Return(mobyclient.ContainerListResult{}, nil)
	cr := &containerReference{cli: client, input: &NewContainerInput{Name: "job-1"}}
	check := func(op string, err error) {
		t.Helper()
		require.ErrorIs(t, err, ErrContainerNotFound, op)
		assert.Contains(t, err.Error(), `container "job-1" does not exist`, op)
	}
	check("copyContent", cr.copyContent("/var/run/act", &FileEntry{Name: "x", Mode: 0o644})(ctx))
	check("copyDir", cr.copyDir("/var/run/act", "/src", false, false)(ctx))
	check("exec", cr.exec([]string{"echo"}, nil, "", "")(ctx))
	_, err := cr.GetContainerArchive(ctx, "/var/run/act/x")
	check("GetContainerArchive", err)
	_, err = cr.Inspect(ctx)
	check("Inspect", err)

	// a known id the daemon has since dropped
	client.On("ContainerInspect", ctx, "gone", mobyclient.ContainerInspectOptions{}).
		Return(mobyclient.ContainerInspectResult{}, cerrdefs.ErrNotFound)
	removed := &containerReference{id: "gone", cli: client, input: &NewContainerInput{Name: "job-1"}}
	_, err = removed.Inspect(ctx)
	check("Inspect after removal", err)
}

// End-to-end: a stale cr.id is cleared, repopulated from name lookup,
// and the Copy completes against the fresh id.
func TestPublicCopyPipelineHandlesStaleID(t *testing.T) {
	ctx := context.Background()
	client := &mockDockerClient{}
	client.On("ContainerInspect", ctx, "stale", mobyclient.ContainerInspectOptions{}).
		Return(mobyclient.ContainerInspectResult{}, cerrdefs.ErrNotFound.WithMessage("gone"))
	client.On("ContainerList", ctx, mobyclient.ContainerListOptions{All: true}).
		Return(mobyclient.ContainerListResult{Items: []container.Summary{
			{ID: "fresh", Names: []string{"/job-1"}},
		}}, nil)
	client.On("CopyToContainer", ctx, "fresh", mock.MatchedBy(func(opts mobyclient.CopyToContainerOptions) bool {
		return opts.DestinationPath == "/var/run/act"
	})).Return(mobyclient.CopyToContainerResult{}, nil)

	cr := &containerReference{id: "stale", cli: client, input: &NewContainerInput{Name: "job-1"}}
	require.NoError(t, cr.Copy("/var/run/act", &FileEntry{Name: "x", Mode: 0o644})(ctx))
	assert.Equal(t, "fresh", cr.id)
	client.AssertExpectations(t)
}

// Type assert containerReference implements ExecutionsEnvironment
var _ ExecutionsEnvironment = &containerReference{}

func TestCheckVolumes(t *testing.T) {
	testCases := []struct {
		desc          string
		validVolumes  []string
		binds         []string
		expectedBinds []string
	}{
		{
			desc:         "match all volumes",
			validVolumes: []string{"**"},
			binds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
			expectedBinds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
		},
		{
			desc:         "no volumes can be matched",
			validVolumes: []string{},
			binds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
			expectedBinds: []string{},
		},
		{
			desc: "only allowed volumes can be matched",
			validVolumes: []string{
				"shared_volume",
				"/home/test/data",
				"/etc/conf.d/*.json",
			},
			binds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
			expectedBinds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			logger, hook := test.NewNullLogger()
			ctx := common.WithLogger(context.Background(), logger)
			cr := &containerReference{
				input: &NewContainerInput{
					ValidVolumes: tc.validVolumes,
				},
			}
			_, hostConf := cr.sanitizeConfig(ctx, &container.Config{}, &container.HostConfig{Binds: tc.binds})
			assert.Equal(t, tc.expectedBinds, hostConf.Binds)
			assert.Len(t, hook.AllEntries(), len(tc.binds)-len(tc.expectedBinds)) // every drop is warned about
		})
	}
}

// A volume driver decides for itself what it mounts, e.g. the local driver with device= binds
// any host path, which valid_volumes never gets to see.
func TestMergeContainerConfigsDropsVolumeDriversFromWorkflows(t *testing.T) {
	const escape = "--mount type=volume,src=job-escape,dst=/host,volume-driver=local,volume-opt=type=none,volume-opt=o=bind,volume-opt=device=/"

	hostConfig, _ := mergeOptions(t, "", escape+" --mount type=volume,src=job-plain,dst=/cache", false)
	require.Len(t, hostConfig.Mounts, 1)
	assert.Equal(t, "job-plain", hostConfig.Mounts[0].Source)

	// the same mount from the runner's own options is the administrator's to make
	hostConfig, _ = mergeOptions(t, escape, "", false)
	require.Len(t, hostConfig.Mounts, 1)
	assert.Equal(t, "job-escape", hostConfig.Mounts[0].Source)
}

// Both of these are read here, on the runner, so a workflow could read the runner's files
// and environment with them.
func TestMergeContainerConfigsKeepsTheRunnersFilesAndEnvToItself(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "host.env")
	require.NoError(t, os.WriteFile(hostFile, []byte("STOLEN=from-the-host\n"), 0o600))
	t.Setenv("RUNNER_SECRET", "s3cr3t")

	for _, option := range []string{"--env-file " + hostFile, "--label-file " + hostFile} {
		logger, _ := test.NewNullLogger()
		cr := &containerReference{input: &NewContainerInput{NetworkMode: "bridge", WorkflowOptions: option}}

		_, _, err := cr.mergeContainerConfigs(common.WithLogger(context.Background(), logger), &container.Config{}, &container.HostConfig{})
		require.ErrorContains(t, err, "not allowed in a workflow")

		// the runner reading its own files is what those options are for
		cr = &containerReference{input: &NewContainerInput{NetworkMode: "bridge", RunnerOptions: option}}
		_, _, err = cr.mergeContainerConfigs(common.WithLogger(context.Background(), logger), &container.Config{}, &container.HostConfig{})
		require.NoError(t, err)
	}

	// a bare name is no longer resolved from the runner's environment, for either source
	logger, _ := test.NewNullLogger()
	cr := &containerReference{input: &NewContainerInput{
		NetworkMode:     "bridge",
		RunnerOptions:   "--env RUNNER_SECRET",
		WorkflowOptions: "--env RUNNER_SECRET --env GIVEN=value",
	}}

	config, _, err := cr.mergeContainerConfigs(common.WithLogger(context.Background(), logger), &container.Config{}, &container.HostConfig{})
	require.NoError(t, err)
	assert.Equal(t, []string{"RUNNER_SECRET", "RUNNER_SECRET", "GIVEN=value"}, config.Env)
}

func TestSanitizeOptionsHostConfig(t *testing.T) {
	logger, _ := test.NewNullLogger()

	// every field the sanitizer resets, so a reset dropped in a refactor fails here
	hostConfig := &container.HostConfig{
		PidMode:           "host",
		IpcMode:           "host",
		UTSMode:           "host",
		CgroupnsMode:      "host",
		UsernsMode:        "host",
		CapAdd:            []string{"ALL"},
		SecurityOpt:       []string{"seccomp=unconfined", "apparmor=unconfined"},
		VolumesFrom:       []string{"other"},
		Runtime:           "runc",
		Isolation:         "process",
		VolumeDriver:      "rogue",
		MaskedPaths:       []string{},
		ReadonlyPaths:     []string{},
		CgroupParent:      "/custom",
		Devices:           []container.DeviceMapping{{PathOnHost: "/dev/sda", PathInContainer: "/dev/sda", CgroupPermissions: "rwm"}},
		DeviceCgroupRules: []string{"a *:* rwm"},
		DeviceRequests:    []container.DeviceRequest{{Count: -1, Capabilities: [][]string{{"gpu"}}}},
		Sysctls:           map[string]string{"net.ipv4.ip_forward": "1"},
	}

	sanitizeOptionsHostConfig(logger, hostConfig, &container.HostConfig{})

	assert.Equal(t, &container.HostConfig{}, hostConfig)
}

// mergeOptions merges both option sources into a bare container, returning the result and its log.
func mergeOptions(t *testing.T, runnerOptions, workflowOptions string, privileged bool) (*container.HostConfig, *test.Hook) {
	t.Helper()
	logger, hook := test.NewNullLogger()
	cr := &containerReference{input: &NewContainerInput{
		RunnerOptions:   runnerOptions,
		WorkflowOptions: workflowOptions,
		NetworkMode:     "bridge",
		UsernsMode:      "private",
	}}

	_, hostConfig, err := cr.mergeContainerConfigs(common.WithLogger(context.Background(), logger), &container.Config{}, &container.HostConfig{
		Privileged:  privileged,
		UsernsMode:  container.UsernsMode("private"),
		NetworkMode: container.NetworkMode("bridge"),
	})
	require.NoError(t, err)
	return hostConfig, hook
}

func TestMergeContainerConfigsStripsDangerousOptionsWhenUnprivileged(t *testing.T) {
	// OS-independent options only, --device and --gpus need a linux/windows server OS
	const dangerousOptions = "--pid=host --ipc=host --uts=host --cgroupns=host " +
		"--userns=host --cap-add=ALL --security-opt seccomp=unconfined " +
		"--security-opt apparmor=unconfined --volumes-from other --isolation process " +
		"--runtime runc --cgroup-parent /custom --sysctl net.ipv4.ip_forward=1"

	// whatever the workflow adds, an unprivileged container comes out exactly as the runner's
	// own options alone describe it, field for field
	for _, runnerOptions := range []string{"--shm-size 1g", dangerousOptions, "--cap-add SYS_ADMIN --security-opt seccomp=unconfined"} {
		runnerOnly, _ := mergeOptions(t, runnerOptions, "", false)
		withWorkflow, _ := mergeOptions(t, runnerOptions, dangerousOptions, false)

		assert.Equal(t, runnerOnly, withWorkflow, "runner options: %q", runnerOptions)
	}

	// the same options from the runner reach the daemon, even --userns, which no workflow may set
	kept, _ := mergeOptions(t, dangerousOptions, "", false)
	assert.Equal(t, "host", string(kept.PidMode))
	assert.Equal(t, []string{"ALL"}, kept.CapAdd)
	assert.Equal(t, "runc", kept.Runtime)
	assert.Equal(t, "host", string(kept.UsernsMode))
	assert.False(t, kept.Privileged)

	// privileged is the administrator opting in, so the workflow's options are honored
	privileged, _ := mergeOptions(t, "", dangerousOptions, true)
	assert.Equal(t, "host", string(privileged.PidMode))
	assert.Equal(t, []string{"ALL"}, privileged.CapAdd)
	assert.Equal(t, []string{"seccomp=unconfined", "apparmor=unconfined"}, privileged.SecurityOpt)
}

func TestCheckVolumesRejectsEscapingHostPaths(t *testing.T) {
	logger, _ := test.NewNullLogger()
	ctx := common.WithLogger(context.Background(), logger)

	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	denied := filepath.Join(base, "denied")
	require.NoError(t, os.MkdirAll(allowed, 0o700))
	require.NoError(t, os.MkdirAll(denied, 0o700))

	cr := &containerReference{
		input: &NewContainerInput{
			ValidVolumes: []string{filepath.Join(allowed, "**")},
		},
	}

	escapingPath := allowed + string(filepath.Separator) + ".." + string(filepath.Separator) + "denied"
	_, hostConf := cr.sanitizeConfig(ctx, &container.Config{}, &container.HostConfig{
		Binds: []string{escapingPath + ":/mnt"},
	})
	assert.Empty(t, hostConf.Binds)

	linkPath := filepath.Join(allowed, "link")
	if err := os.Symlink(denied, linkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	_, hostConf = cr.sanitizeConfig(ctx, &container.Config{}, &container.HostConfig{
		Binds: []string{linkPath + ":/mnt"},
	})
	assert.Empty(t, hostConf.Binds)

	_, hostConf = cr.sanitizeConfig(ctx, &container.Config{}, &container.HostConfig{
		Binds: []string{filepath.Join(linkPath, "missing") + ":/mnt"},
	})
	assert.Empty(t, hostConf.Binds)
}

func TestContainerInfoFromInspect(t *testing.T) {
	t.Run("reports no healthcheck when the image declares none", func(t *testing.T) {
		info := containerInfoFromInspect(container.InspectResponse{
			ID:    "abc123",
			State: &container.State{Status: "running", Running: true},
		})

		assert.Equal(t, "abc123", info.ID)
		assert.Equal(t, "running", info.State)
		assert.Equal(t, HealthNone, info.Health)
		assert.Empty(t, info.Ports)
	})

	t.Run("reports the health status and the last probe output", func(t *testing.T) {
		info := containerInfoFromInspect(container.InspectResponse{
			State: &container.State{
				Status: "running",
				Health: &container.Health{
					Status: container.Unhealthy,
					Log: []*container.HealthcheckResult{
						{Output: "first\n"},
						{Output: "connection refused\n"},
					},
				},
			},
		})

		assert.Equal(t, HealthUnhealthy, info.Health)
		assert.Equal(t, "connection refused", info.HealthOutput)
	})

	t.Run("reports the published ports", func(t *testing.T) {
		info := containerInfoFromInspect(container.InspectResponse{
			State: &container.State{Status: "running"},
			NetworkSettings: &container.NetworkSettings{
				Ports: network.PortMap{
					network.MustParsePort("5432/tcp"): []network.PortBinding{{HostPort: "49153"}},
					network.MustParsePort("6379/tcp"): nil,
				},
			},
		})

		assert.Equal(t, map[string]string{"5432": "49153"}, info.Ports)
	})

	t.Run("tolerates a container without state", func(t *testing.T) {
		info := containerInfoFromInspect(container.InspectResponse{ID: "abc123"})

		assert.Equal(t, "abc123", info.ID)
		assert.Equal(t, HealthNone, info.Health)
	})

	t.Run("reports the mount sources by container path", func(t *testing.T) {
		info := containerInfoFromInspect(container.InspectResponse{
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "job", Source: "/var/lib/docker/volumes/job/_data", Destination: "/workspace/owner/repo"},
				{Type: mount.TypeBind, Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
				{Type: mount.TypeVolume, Name: "cache", Source: "/var/lib/docker/volumes/cache/_data", Destination: "/cache"},
				{Type: mount.TypeBind, Source: "/custom/resolv.conf", Destination: "/etc/resolv.conf"},
			},
			HostConfig: &container.HostConfig{
				Tmpfs:  map[string]string{"/workspace/owner/repo//tmp/": "size=1m"},
				Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "cache", Target: "/cache/", VolumeOptions: &mount.VolumeOptions{Subpath: "project"}}},
			},
		})

		assert.Equal(t, map[string]string{
			"/workspace/owner/repo":     "/var/lib/docker/volumes/job/_data",
			"/workspace/owner/repo/tmp": "",
			"/var/run/docker.sock":      "/var/run/docker.sock",
			"/cache":                    "/var/lib/docker/volumes/cache/_data/project",
			"/etc/resolv.conf":          "/custom/resolv.conf",
			"/etc/hosts":                "",
			"/etc/hostname":             "",
		}, info.Mounts)
	})
}

func TestMergeContainerConfigsVolumesReplaceRunnerMounts(t *testing.T) {
	logger, _ := test.NewNullLogger()
	ctx := common.WithLogger(context.Background(), logger)
	cr := &containerReference{
		input: &NewContainerInput{
			NetworkMode:   "bridge",
			RunnerOptions: "--volume /host/tools:/opt/hostedtoolcache",
		},
	}

	_, hostConf, err := cr.mergeContainerConfigs(ctx, &container.Config{}, &container.HostConfig{
		Binds:  []string{"/var/run/docker.sock:/var/run/docker.sock"},
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "act-toolcache", Target: "/opt/hostedtoolcache"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/var/run/docker.sock:/var/run/docker.sock", "/host/tools:/opt/hostedtoolcache"}, hostConf.Binds)
	assert.Empty(t, hostConf.Mounts)
}

func TestMergeContainerConfigsWarnsOnlyAboutOptionsThatWereGiven(t *testing.T) {
	warnings := func(runnerOptions, workflowOptions string) int {
		_, hook := mergeOptions(t, runnerOptions, workflowOptions, false)
		return len(hook.AllEntries())
	}

	assert.Zero(t, warnings("--volume /host/tools:/opt/hostedtoolcache", ""))
	assert.Zero(t, warnings("", "--shm-size 1g"))
	assert.Equal(t, 1, warnings("--network host", ""))
}

func TestMergeContainerConfigsKeepsNetworkAliasesFromOptions(t *testing.T) {
	logger, _ := test.NewNullLogger()
	cr := &containerReference{input: &NewContainerInput{
		NetworkMode: "job-network", NetworkAliases: []string{"redis"}, WorkflowOptions: "--network-alias redis-primary",
	}}

	_, _, err := cr.mergeContainerConfigs(common.WithLogger(t.Context(), logger), &container.Config{}, &container.HostConfig{})
	require.NoError(t, err)
	assert.Equal(t, []string{"redis", "redis-primary"}, cr.input.NetworkAliases)
}

// A dead daemon must fail the job, not panic through logrus and not silently
// drop the requested platform.
func TestSupportsContainerImagePlatformDaemonError(t *testing.T) {
	cli := &mockDockerClient{}
	cli.On("ServerVersion", mock.Anything, mock.Anything).
		Return(mobyclient.ServerVersionResult{}, errors.New("cannot connect to the Docker daemon"))

	supported, err := supportsContainerImagePlatform(t.Context(), cli)
	require.ErrorContains(t, err, "cannot connect to the Docker daemon")
	assert.False(t, supported)
}
