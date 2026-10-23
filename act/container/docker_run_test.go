// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDockerEnvironmentUsesContainerPath(t *testing.T) {
	client := &mockDockerClient{}
	client.On("ContainerInspect", mock.Anything, "amd64-job", mobyclient.ContainerInspectOptions{}).
		Return(mobyclient.ContainerInspectResult{Container: container.InspectResponse{Config: &container.Config{Env: []string{
			"PATH=/opt/node/x64/bin:/usr/bin:/bin", "CONFLICT_VAR=container-value", "SOME_RANDOM_VAR=",
			"ANOTHER_ONE=old-value", "ANOTHER_ONE=BUT_I_HAVE_VALUE", "LITERAL='quoted'=$HOME\nsecond line",
		}}}}, nil)
	env := map[string]string{
		"PATH":         "/custom/bin",
		"RANDOM_VAR":   "WITH_VALUE",
		"ANOTHER_VAR":  "",
		"CONFLICT_VAR": "step-value",
	}
	require.NoError(t, (&containerReference{cli: client, id: "amd64-job"}).UpdateFromImageEnv(&env)(t.Context()))
	assert.Equal(t, map[string]string{
		"PATH":            "/custom/bin:/opt/node/x64/bin:/usr/bin:/bin",
		"RANDOM_VAR":      "WITH_VALUE",
		"ANOTHER_VAR":     "",
		"SOME_RANDOM_VAR": "",
		"ANOTHER_ONE":     "BUT_I_HAVE_VALUE",
		"CONFLICT_VAR":    "step-value",
		"LITERAL":         "'quoted'=$HOME\nsecond line",
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

func (m *mockDockerClient) ContainerLogs(ctx context.Context, containerID string, opts mobyclient.ContainerLogsOptions) (mobyclient.ContainerLogsResult, error) {
	args := m.Called(ctx, containerID, opts)
	return args.Get(0).(mobyclient.ContainerLogsResult), args.Error(1)
}

func (m *mockDockerClient) ContainerStart(ctx context.Context, containerID string, opts mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error) {
	args := m.Called(ctx, containerID, opts)
	return args.Get(0).(mobyclient.ContainerStartResult), args.Error(1)
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

func (m *mockDockerClient) NetworkCreate(ctx context.Context, name string, opts mobyclient.NetworkCreateOptions) (mobyclient.NetworkCreateResult, error) {
	args := m.Called(ctx, name, opts)
	return args.Get(0).(mobyclient.NetworkCreateResult), args.Error(1)
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
	closed atomic.Bool
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	args := m.Called(b)
	return args.Int(0), args.Error(1)
}

func (m *mockConn) Close() (err error) {
	m.closed.Store(true)
	return nil
}

func TestDockerExecAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

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
		id:    "123",
		cli:   client,
		input: &NewContainerInput{},
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
	ctx := t.Context()

	client := &mockDockerClient{}
	client.On("ExecCreate", ctx, "123", mock.AnythingOfType("client.ExecCreateOptions")).Return(mobyclient.ExecCreateResult{ID: "id"}, nil)
	client.On("ExecAttach", ctx, "id", mock.AnythingOfType("client.ExecAttachOptions")).Return(mobyclient.ExecAttachResult{
		Conn:   &mockConn{},
		Reader: bufio.NewReader(strings.NewReader("output")),
	}, nil)
	client.On("ExecInspect", ctx, "id", mobyclient.ExecInspectOptions{}).Return(mobyclient.ExecInspectResult{
		ExitCode: 1,
	}, nil)

	cr := &containerReference{
		id:    "123",
		cli:   client,
		input: &NewContainerInput{},
	}

	err := cr.exec([]string{""}, map[string]string{}, "user", "workdir")(ctx)
	var exitErr ExitCodeError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, ExitCodeError(1), exitErr)
	assert.Equal(t, "Process completed with exit code 1.", err.Error())

	client.AssertExpectations(t)
}

func TestDockerExecStreamFailure(t *testing.T) {
	ctx := t.Context()
	client := &mockDockerClient{}
	client.On("ExecCreate", ctx, "123", mock.AnythingOfType("client.ExecCreateOptions")).
		Return(mobyclient.ExecCreateResult{ID: "id"}, nil)
	client.On("ExecAttach", ctx, "id", mock.AnythingOfType("client.ExecAttachOptions")).
		Return(mobyclient.ExecAttachResult{
			Conn:   &mockConn{},
			Reader: bufio.NewReader(bytes.NewReader(stdcopyFrame(stdcopy.Systemerr, "unable to start exec"))),
		}, nil)
	require.ErrorContains(t, (&containerReference{id: "123", cli: client, input: &NewContainerInput{}}).
		exec([]string{"sh"}, nil, "", "")(ctx), "unable to start exec")
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

func TestDockerAttachFlushesTrailingLine(t *testing.T) {
	for _, testcase := range []struct {
		name string
		tty  bool
	}{
		{name: "multiplexed"},
		{name: "tty from container options", tty: true},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			ctx := t.Context()
			output := []byte("line one\nlast line without newline")
			if !testcase.tty {
				output = stdcopyFrame(stdcopy.Stdout, string(output))
			}
			var lines []string
			logWriter := common.NewLineWriter(func(line string) bool {
				lines = append(lines, line)
				return true
			})
			client := &mockDockerClient{}
			client.On("ContainerInspect", ctx, "123", mobyclient.ContainerInspectOptions{}).
				Return(mobyclient.ContainerInspectResult{Container: container.InspectResponse{Config: &container.Config{Tty: testcase.tty}}}, nil)
			conn := &mockConn{}
			client.On("ContainerAttach", ctx, "123", mock.AnythingOfType("client.ContainerAttachOptions")).
				Return(mobyclient.ContainerAttachResult{
					Conn:   conn,
					Reader: bufio.NewReader(bytes.NewReader(output)),
				}, nil)
			client.On("ContainerLogs", ctx, "123", mobyclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true}).
				Return(io.NopCloser(bytes.NewReader(output)), nil)
			statusCh := make(chan container.WaitResponse, 1)
			statusCh <- container.WaitResponse{StatusCode: 0}
			client.On("ContainerWait", ctx, "123", mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning}).
				Return(mobyclient.ContainerWaitResult{Result: statusCh})
			cr := &containerReference{id: "123", cli: client, input: &NewContainerInput{Stdout: logWriter, Stderr: logWriter}}

			require.NoError(t, cr.attach()(ctx))
			require.NoError(t, cr.wait()(ctx))
			assert.Equal(t, []string{"line one\n", "last line without newline"}, lines)
			assert.True(t, conn.closed.Load())
			require.NoError(t, cr.DumpLogs(ctx))
			assert.Equal(t, []string{"line one\n", "last line without newline", "line one\n", "last line without newline"}, lines)
			client.AssertExpectations(t)
		})
	}
}

func TestDockerAttachStreamClosesWithItsContext(t *testing.T) {
	conn, peer := net.Pipe()
	t.Cleanup(func() { _ = conn.Close() })
	t.Cleanup(func() { _ = peer.Close() })
	client := &mockDockerClient{}
	client.On("ContainerInspect", mock.Anything, "123", mobyclient.ContainerInspectOptions{}).
		Return(mobyclient.ContainerInspectResult{Container: container.InspectResponse{Config: &container.Config{}}}, nil)
	client.On("ContainerAttach", mock.Anything, "123", mock.AnythingOfType("client.ContainerAttachOptions")).
		Return(mobyclient.ContainerAttachResult{HijackedResponse: mobyclient.NewHijackedResponse(conn, "")}, nil)
	client.On("ContainerStart", mock.Anything, "123", mobyclient.ContainerStartOptions{}).
		Return(mobyclient.ContainerStartResult{}, errors.New("container start failed"))
	cr := &containerReference{id: "123", cli: client, input: &NewContainerInput{}}

	ctx, cancel := context.WithCancel(t.Context())
	require.ErrorContains(t, cr.Start(true)(ctx), "container start failed")
	cancel()
	<-cr.attachDone
	client.AssertExpectations(t)
}

func TestDockerWaitFailure(t *testing.T) {
	ctx := t.Context()
	statusCh := make(chan container.WaitResponse, 1)
	statusCh <- container.WaitResponse{StatusCode: 2}
	client := &mockDockerClient{}
	client.On("ContainerWait", ctx, "123", mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning}).
		Return(mobyclient.ContainerWaitResult{Result: statusCh})
	cr := &containerReference{id: "123", cli: client}

	err := cr.wait()(ctx)
	var exitErr ExitCodeError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, ExitCodeError(2), exitErr)
	assert.Equal(t, "Process completed with exit code 2.", err.Error())
	statusCh <- container.WaitResponse{Error: &container.WaitExitError{Message: "container wait failed"}}
	require.EqualError(t, cr.wait()(ctx), "failed to wait for container: container wait failed")
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
			logger, hook := logtest.NewLogger()
			ctx := common.WithLogger(t.Context(), logger)
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
			assert.Empty(t, hook.AllRecords())
			client.AssertExpectations(t)
		})
	}
}

// A container whose id was never learned, because find() could not reach the daemon or
// create() lost its reply, must still be removed rather than leaking with its network. It
// was never started here, so it is not worth a kill of its own.
func TestRemoveWithoutIDUsesName(t *testing.T) {
	ctx := t.Context()
	client := &mockDockerClient{}
	client.On("ContainerRemove", ctx, "job-1", mobyclient.ContainerRemoveOptions{RemoveVolumes: true, Force: true}).
		Return(mobyclient.ContainerRemoveResult{}, nil)
	cr := &containerReference{cli: client, input: &NewContainerInput{Name: "job-1"}}

	require.NoError(t, cr.remove()(ctx))
	client.AssertExpectations(t)
}

func TestRemovePreservesClientCreationError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "://")

	require.ErrorContains(t, (&containerReference{input: &NewContainerInput{Name: "job-1"}}).
		Remove()(t.Context()), "failed to connect to docker daemon")
}

func TestFindRevalidatesStaleID(t *testing.T) {
	ctx := t.Context()
	inspectOpts := mobyclient.ContainerInspectOptions{}
	notFound := cerrdefs.ErrNotFound.WithMessage("No such container")
	boom := errors.New("daemon unreachable")
	for _, tc := range []struct {
		name, cachedID string
		cachedErr      error
		lookup         container.InspectResponse
		lookupErr      error
		listed         []container.Summary
		wantID         string
		retryList      bool
	}{
		{name: "stale id cleared, name lookup empty", cachedID: "stale", cachedErr: notFound, lookupErr: notFound},
		{name: "stale id cleared, name lookup repopulates", cachedID: "stale", cachedErr: notFound, lookup: container.InspectResponse{ID: "fresh", Name: "/job-1"}, wantID: "fresh"},
		{name: "id match falls back to listing", lookup: container.InspectResponse{ID: "job-1abc", Name: "/somebody-else"}, listed: []container.Summary{{ID: "fresh", Names: []string{"/job-1"}}}, wantID: "fresh"},
		{name: "live id kept", cachedID: "live", wantID: "live"},
		{name: "transient inspect error trusts cache", cachedID: "live", cachedErr: boom, wantID: "live"},
		{name: "name lookup error falls back to listing", lookupErr: boom, listed: []container.Summary{{ID: "other", Names: []string{"/somebody-else"}}, {ID: "fresh", Names: []string{"/job-1"}}}, wantID: "fresh", retryList: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockDockerClient{}
			cr := &containerReference{id: tc.cachedID, cli: client, input: &NewContainerInput{Name: "job-1"}}
			if tc.cachedID != "" {
				client.On("ContainerInspect", ctx, tc.cachedID, inspectOpts).Return(mobyclient.ContainerInspectResult{}, tc.cachedErr)
			}
			if tc.cachedID == "" || cerrdefs.IsNotFound(tc.cachedErr) {
				client.On("ContainerInspect", ctx, "job-1", inspectOpts).Return(mobyclient.ContainerInspectResult{Container: tc.lookup}, tc.lookupErr)
			}
			if tc.listed != nil {
				client.On("ContainerList", ctx, mobyclient.ContainerListOptions{All: true}).Return(mobyclient.ContainerListResult{Items: tc.listed}, nil).Once()
			}
			require.NoError(t, cr.find()(ctx))
			assert.Equal(t, tc.wantID, cr.id)
			if tc.retryList {
				client.On("ContainerList", ctx, mobyclient.ContainerListOptions{All: true}).Return(mobyclient.ContainerListResult{}, boom)
				cr.id = ""
				require.ErrorIs(t, cr.find()(ctx), boom)
			}
			client.AssertExpectations(t)
		})
	}
}

func TestRunnerArchCachedPerRun(t *testing.T) {
	var infos atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("API-Version", "1.47")
		if strings.HasSuffix(request.URL.Path, "/info") {
			infos.Add(1)
			_, _ = io.WriteString(writer, `{"Architecture":"aarch64"}`)
		}
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("DOCKER_HOST", daemon.URL)
	extensions := &LinuxContainerEnvironmentExtensions{}
	run := WithRunnerArch(t.Context())
	var lookups sync.WaitGroup
	for range 4 {
		lookups.Go(func() { assert.Equal(t, "ARM64", extensions.GetRunnerContext(run)["arch"]) })
	}
	lookups.Wait()
	assert.Equal(t, "ARM64", extensions.GetRunnerContext(t.Context())["arch"])
	assert.EqualValues(t, 2, infos.Load())
}

// Every daemon entry point fails fast with a clear, container-named
// error when no live cr.id is known.
func TestRejectsMissingContainer(t *testing.T) {
	ctx := t.Context()
	client := &mockDockerClient{}
	client.On("ContainerInspect", ctx, "job-1", mobyclient.ContainerInspectOptions{}).
		Return(mobyclient.ContainerInspectResult{}, cerrdefs.ErrNotFound)
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

func TestPublicCopyPipelineHandlesStaleID(t *testing.T) {
	for _, testcase := range []struct {
		name, destination, extractionRoot, entry string
		directory                                bool
	}{
		{name: "files", destination: "/var/run/act", extractionRoot: "/var/run/act", entry: "x"},
		{name: "action below act volume", directory: true, destination: "/var/run/act/actions/checkout/", extractionRoot: "/var/run/act", entry: "actions/checkout/x"},
		{name: "sibling prefix", directory: true, destination: "/var/run/actor/action", extractionRoot: "/", entry: "var/run/actor/action/x"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			hook := logtest.NewWithLevel(logging.LevelError)
			ctx := common.WithLogger(t.Context(), logging.New(hook))
			client := &mockDockerClient{}
			client.On("ContainerInspect", ctx, "stale", mobyclient.ContainerInspectOptions{}).
				Return(mobyclient.ContainerInspectResult{}, cerrdefs.ErrNotFound.WithMessage("gone"))
			client.On("ContainerInspect", ctx, "job-1", mobyclient.ContainerInspectOptions{}).
				Return(mobyclient.ContainerInspectResult{Container: container.InspectResponse{ID: "fresh", Name: "/job-1"}}, nil)
			client.On("CopyToContainer", ctx, "fresh", mock.Anything).
				Run(func(args mock.Arguments) {
					opts := args.Get(2).(mobyclient.CopyToContainerOptions)
					assert.Equal(t, testcase.extractionRoot, opts.DestinationPath)
					header, err := tar.NewReader(opts.Content).Next()
					require.NoError(t, err)
					assert.Equal(t, testcase.entry, header.Name)
				}).Return(mobyclient.CopyToContainerResult{}, nil).Once()

			cr := &containerReference{id: "stale", cli: client, input: &NewContainerInput{Name: "job-1"}}
			if testcase.directory {
				source := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(source, "x"), nil, 0o644))
				require.NoError(t, cr.CopyDir(testcase.destination, source+string(filepath.Separator), false, true)(ctx))
			} else {
				require.NoError(t, cr.Copy(testcase.destination, &FileEntry{Name: "x", Mode: 0o644})(ctx))
			}
			assert.Equal(t, "fresh", cr.id)
			assert.Empty(t, hook.AllRecords())
			client.AssertExpectations(t)
		})
	}
}

func TestCheckVolumes(t *testing.T) {
	testCases := []struct {
		desc           string
		validVolumes   []string
		binds          []string
		expectedBinds  []string
		mounts         []mount.Mount
		expectedMounts []mount.Mount
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
				"/opt/**",
			},
			binds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"/etc/conf.d/sub/nested.json:/config/nested.json",
				"/opt/app/cache:/cache",
				"sql_data:/sql_data",
				"/secrets/keys:/keys",
			},
			expectedBinds: []string{
				"shared_volume:/shared_volume",
				"/home/test/data:/test_data",
				"/etc/conf.d/base.json:/config/base.json",
				"/opt/app/cache:/cache",
			},
		},
		{
			desc:          "read-only rules allow only read-only mounts and copy nothing into volumes",
			validVolumes:  []string{"/etc/ssl:ro", "data:ro", "shared"},
			binds:         []string{"/etc/ssl:/ssl:ro", "/etc/ssl:/ssl", "data:/data:ro", "data:/data", "shared:/shared:ro"},
			expectedBinds: []string{"/etc/ssl:/ssl:ro", "data:/data:ro,nocopy", "shared:/shared:ro"},
			mounts: []mount.Mount{
				{Type: mount.TypeBind, Source: "/etc/ssl", Target: "/ssl", ReadOnly: true},
				{Type: mount.TypeBind, Source: "/etc/ssl", Target: "/ssl", ReadOnly: true, BindOptions: &mount.BindOptions{ReadOnlyNonRecursive: true}},
				{Type: mount.TypeVolume, Source: "data", Target: "/data", ReadOnly: true},
			},
			expectedMounts: []mount.Mount{
				{Type: mount.TypeBind, Source: "/etc/ssl", Target: "/ssl", ReadOnly: true},
				{Type: mount.TypeVolume, Source: "data", Target: "/data", ReadOnly: true, VolumeOptions: &mount.VolumeOptions{NoCopy: true}},
			},
		},
		{
			desc:          "read-only wildcard keeps writable rules writable",
			validVolumes:  []string{"**:ro", "shared"},
			binds:         []string{"/etc/ssl:/ssl:ro", "data:/data", "shared:/shared"},
			expectedBinds: []string{"/etc/ssl:/ssl:ro", "shared:/shared"},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			logger, hook := logtest.NewLogger()
			ctx := common.WithLogger(t.Context(), logger)
			cr := &containerReference{
				input: &NewContainerInput{
					ValidVolumes: tc.validVolumes,
				},
			}
			hostConf := &container.HostConfig{Binds: tc.binds, Mounts: tc.mounts}
			cr.sanitizeConfig(ctx, hostConf)
			assert.Equal(t, tc.expectedBinds, hostConf.Binds)
			assert.ElementsMatch(t, tc.expectedMounts, hostConf.Mounts)
			assert.Len(t, hook.AllRecords(), len(tc.binds)+len(tc.mounts)-len(tc.expectedBinds)-len(tc.expectedMounts))
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

func TestMergeContainerConfigsKeepsTheRunnersFilesAndEnvToItself(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "host.env")
	require.NoError(t, os.WriteFile(hostFile, []byte("STOLEN=from-the-host\n"), 0o600))
	t.Setenv("RUNNER_SECRET", "s3cr3t")

	for _, option := range []string{"--env-file " + hostFile, "--label-file " + hostFile} {
		logger := logging.New(logtest.New())
		cr := &containerReference{input: &NewContainerInput{NetworkMode: "bridge", WorkflowOptions: option}}

		require.ErrorContains(t, cr.mergeContainerConfigs(common.WithLogger(t.Context(), logger), &container.Config{}, &container.HostConfig{}), "not allowed in a workflow")

		cr = &containerReference{input: &NewContainerInput{NetworkMode: "bridge", RunnerOptions: option}}
		require.NoError(t, cr.mergeContainerConfigs(common.WithLogger(t.Context(), logger), &container.Config{}, &container.HostConfig{}))
	}

	logger := logging.New(logtest.New())
	cr := &containerReference{input: &NewContainerInput{
		NetworkMode:     "bridge",
		RunnerOptions:   "--env RUNNER_SECRET",
		WorkflowOptions: "--env RUNNER_SECRET --env GIVEN=value",
	}}

	config := &container.Config{}
	require.NoError(t, cr.mergeContainerConfigs(common.WithLogger(t.Context(), logger), config, &container.HostConfig{}))
	assert.Equal(t, []string{"RUNNER_SECRET", "RUNNER_SECRET", "GIVEN=value"}, config.Env)
}

func TestSanitizeOptionsHostConfig(t *testing.T) {
	logger := logging.New(logtest.New())

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
		LogConfig:         container.LogConfig{Type: "syslog", Config: map[string]string{"syslog-address": "tcp://169.254.169.254:514"}},
		OomScoreAdj:       -1000,
		OomKillDisable:    new(true),
	}

	sanitizeOptionsHostConfig(logger, hostConfig, &container.HostConfig{})

	assert.Equal(t, &container.HostConfig{}, hostConfig)
}

func mergeOptions(t *testing.T, runnerOptions, workflowOptions string, privileged bool) (*container.HostConfig, *logtest.Handler) {
	t.Helper()
	logger, hook := logtest.NewLogger()
	cr := &containerReference{input: &NewContainerInput{
		RunnerOptions:   runnerOptions,
		WorkflowOptions: workflowOptions,
		NetworkMode:     "bridge",
		UsernsMode:      "private",
		ValidVolumes:    []string{"**"},
	}}

	hostConfig := &container.HostConfig{
		Privileged:  privileged,
		UsernsMode:  container.UsernsMode("private"),
		NetworkMode: container.NetworkMode("bridge"),
	}
	require.NoError(t, cr.mergeContainerConfigs(common.WithLogger(t.Context(), logger), &container.Config{}, hostConfig))
	return hostConfig, hook
}

func TestMergeContainerConfigsStripsDangerousOptionsWhenUnprivileged(t *testing.T) {
	// OS-independent options only, --device and --gpus need a linux/windows server OS
	const dangerousOptions = "--pid=host --ipc=host --uts=host --cgroupns=host " +
		"--userns=host --cap-add=ALL --security-opt seccomp=unconfined " +
		"--security-opt apparmor=unconfined --volumes-from other --isolation process " +
		"--runtime runc --cgroup-parent /custom --sysctl net.ipv4.ip_forward=1 " +
		"--log-driver=syslog --log-opt syslog-address=tcp://169.254.169.254:514 " +
		"--oom-score-adj=-1000 --oom-kill-disable"

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
	logger := logging.New(logtest.New())
	ctx := common.WithLogger(t.Context(), logger)

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
	hostConf := &container.HostConfig{
		Binds: []string{escapingPath + ":/mnt"},
	}
	cr.sanitizeConfig(ctx, hostConf)
	assert.Empty(t, hostConf.Binds)

	linkPath := filepath.Join(allowed, "link")
	if err := os.Symlink(denied, linkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	hostConf = &container.HostConfig{
		Binds: []string{linkPath + ":/mnt"},
	}
	cr.sanitizeConfig(ctx, hostConf)
	assert.Empty(t, hostConf.Binds)

	hostConf = &container.HostConfig{
		Binds: []string{filepath.Join(linkPath, "missing") + ":/mnt"},
	}
	cr.sanitizeConfig(ctx, hostConf)
	assert.Empty(t, hostConf.Binds)
}

func TestSanitizeConfigKeepsTmpfsMounts(t *testing.T) {
	hostConfig, _ := mergeOptions(t, "", "--mount type=tmpfs,dst=/var/lib/mysql", false)
	require.Len(t, hostConfig.Mounts, 1)

	for _, validVolumes := range [][]string{nil, {"data"}, {"**:ro"}} {
		logger, hook := logtest.NewLogger()
		sanitized := &container.HostConfig{Mounts: []mount.Mount{
			hostConfig.Mounts[0],
			{Type: mount.TypeVolume, Target: "/anonymous"},
			{Type: mount.TypeTmpfs, Source: "/etc", Target: "/etc-copy"},
		}}
		(&containerReference{input: &NewContainerInput{ValidVolumes: validVolumes}}).sanitizeConfig(common.WithLogger(t.Context(), logger), sanitized)
		assert.Equal(t, hostConfig.Mounts, sanitized.Mounts)
		assert.Len(t, hook.AllRecords(), 2)
	}
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
	logger := logging.New(logtest.New())
	ctx := common.WithLogger(t.Context(), logger)
	cr := &containerReference{
		input: &NewContainerInput{
			NetworkMode:     "bridge",
			RunnerOptions:   "--volume /host/tools:/opt/hostedtoolcache",
			WorkflowOptions: "--volume /unapproved:/var/run/docker.sock",
			ValidVolumes:    []string{"/host/tools"},
		},
	}

	hostConf := &container.HostConfig{
		Binds:  []string{"/var/run/docker.sock:/var/run/docker.sock"},
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "act-toolcache", Target: "/opt/hostedtoolcache"}},
	}
	require.NoError(t, cr.mergeContainerConfigs(ctx, &container.Config{}, hostConf))
	assert.Equal(t, []string{"/var/run/docker.sock:/var/run/docker.sock", "/host/tools:/opt/hostedtoolcache"}, hostConf.Binds)
	assert.Empty(t, hostConf.Mounts)
}

func TestMergeContainerConfigsWarnsOnlyAboutOptionsThatWereGiven(t *testing.T) {
	warnings := func(runnerOptions, workflowOptions string) int {
		_, hook := mergeOptions(t, runnerOptions, workflowOptions, false)
		return len(hook.AllRecords())
	}

	assert.Zero(t, warnings("--volume /host/tools:/opt/hostedtoolcache", ""))
	assert.Zero(t, warnings("", "--shm-size 1g"))
	assert.Equal(t, 1, warnings("--network host", ""))
	assert.Equal(t, 1, warnings("", "--privileged"))
}

func TestDockerCreateKeepsInputEntrypoint(t *testing.T) {
	for _, entrypoint := range [][]string{nil, {"sleep", "3600"}} {
		for _, optionEntrypoint := range []string{"/sbin/dumb-init", ""} {
			logger, hook := logtest.NewLogger()
			cr := &containerReference{
				input: &NewContainerInput{
					NetworkMode:     "bridge",
					Entrypoint:      entrypoint,
					RunnerOptions:   "--entrypoint=runner-entrypoint",
					WorkflowOptions: "--entrypoint=" + optionEntrypoint + " --init",
				},
				cli: &probeClient{create: func(opts mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
					if len(entrypoint) > 0 {
						assert.Equal(t, entrypoint, opts.Config.Entrypoint)
					} else {
						assert.Equal(t, []string{optionEntrypoint}, opts.Config.Entrypoint)
					}
					require.NotNil(t, opts.HostConfig.Init)
					assert.True(t, *opts.HostConfig.Init)
					return mobyclient.ContainerCreateResult{ID: "created"}, nil
				}},
			}
			require.NoError(t, cr.create(nil, nil)(common.WithLogger(t.Context(), logger)))
			assert.Equal(t, "created", cr.id)
			assert.Equal(t, len(entrypoint) > 0, len(hook.AllRecords()) == 1)
		}
	}
}

func TestMergeContainerConfigsKeepsNetworkAliasesFromOptions(t *testing.T) {
	logger := logging.New(logtest.New())
	cr := &containerReference{input: &NewContainerInput{
		NetworkMode: "job-network", NetworkAliases: []string{"redis"}, WorkflowOptions: "--network-alias redis-primary",
	}}

	require.NoError(t, cr.mergeContainerConfigs(common.WithLogger(t.Context(), logger), &container.Config{}, &container.HostConfig{}))
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
