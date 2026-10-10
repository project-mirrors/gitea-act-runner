// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gitea.com/gitea/runner/internal/pkg/config"
	"gitea.com/gitea/runner/internal/pkg/report"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"gitea.dev/actionslib/runner/v1/runnerv1connect"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestResolveLabels(t *testing.T) {
	var (
		cfgLabels = []string{"cfg:host"}
		regLabels = []string{"reg:host"}
	)

	tests := []struct {
		name string
		arg  string
		cfg  []string
		reg  []string
		want []string
	}{
		{"flag wins", "flag:host,other", cfgLabels, regLabels, []string{"flag:host", "other"}},
		{"config wins over registration", "", cfgLabels, regLabels, cfgLabels},
		{"registration is the fallback", "", nil, regLabels, regLabels},
		{"blank flag is ignored", " , ", cfgLabels, regLabels, cfgLabels},
		{"nothing configured", "", nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, resolveLabels(tt.arg, tt.cfg, tt.reg))
		})
	}
}

func TestGetDockerSocketPathUsesConfigAndEnvironment(t *testing.T) {
	got, err := getDockerSocketPath("tcp://docker.example:2376")
	require.NoError(t, err)
	require.Equal(t, "tcp://docker.example:2376", got)

	t.Setenv("DOCKER_HOST", "unix:///tmp/docker.sock")
	got, err = getDockerSocketPath("-")
	require.NoError(t, err)
	require.Equal(t, "unix:///tmp/docker.sock", got)
}

func TestInitLoggingSetsLevelAndCaller(t *testing.T) {
	oldLevel := log.GetLevel()
	oldReportCaller := log.StandardLogger().ReportCaller
	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.SetReportCaller(oldReportCaller)
	})

	oldFormatter := log.StandardLogger().Formatter
	t.Cleanup(func() { log.SetFormatter(oldFormatter) })

	cfg := &config.Config{}
	cfg.Log.Level = "debug"
	initLogging(cfg)

	require.Equal(t, log.DebugLevel, log.GetLevel())
	require.True(t, log.StandardLogger().ReportCaller)
	// act plans a job on this logger, so a live task's secrets have to be masked out of it
	require.IsType(t, report.MaskingFormatter(nil), log.StandardLogger().Formatter)
}

var errUnregisteredRunner = errors.New("rpc error: code = Unauthenticated desc = unregistered runner")

type daemonTestService struct {
	runnerv1connect.UnimplementedRunnerServiceHandler
	declareErr, fetchErr error
	declareCalls         atomic.Int32
	beforeTask           bool
	cancel               context.CancelFunc
}

func (s *daemonTestService) Declare(context.Context, *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error) {
	if s.declareCalls.Add(1) == 1 {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("bad gateway"))
	}
	if s.declareErr != nil {
		if s.beforeTask {
			s.cancel()
		}
		return nil, s.declareErr
	}
	return connect.NewResponse(&runnerv1.DeclareResponse{Runner: &runnerv1.Runner{Name: "test"}}), nil
}

func (s *daemonTestService) FetchTask(context.Context, *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	if s.fetchErr != nil {
		return nil, s.fetchErr
	}
	if s.beforeTask {
		s.cancel()
		return nil, context.Canceled
	}
	return connect.NewResponse(&runnerv1.FetchTaskResponse{Task: &runnerv1.Task{Id: 1, Context: &structpb.Struct{}, WorkflowPayload: []byte("invalid: [")}}), nil
}

func (*daemonTestService) UpdateTask(_ context.Context, req *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	return connect.NewResponse(&runnerv1.UpdateTaskResponse{State: req.Msg.State}), nil
}

func (*daemonTestService) UpdateLog(_ context.Context, req *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	return connect.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: req.Msg.Index + int64(len(req.Msg.Rows))}), nil
}

func TestDaemonRetriesDeclareOutagesAndRemovesOnlyConsumedEphemeralRegistration(t *testing.T) {
	defer func(delay time.Duration) { declareRetryDelay = delay }(declareRetryDelay)
	declareRetryDelay = 0
	for _, tc := range []struct {
		name                  string
		ephemeral, beforeTask bool
		declareErr, fetchErr  error
		removed               bool
	}{
		{"ephemeral failed task", true, false, nil, nil, true},
		{"ephemeral interrupted before task", true, true, nil, nil, false},
		{"persistent once", false, false, nil, nil, false},
		{"ephemeral rejected on fetch", true, false, nil, errUnregisteredRunner, true},
		{"ephemeral rejected on fetch with 401", true, false, nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unregistered runner")), true},
		{"ephemeral proxy 401 on fetch", true, false, nil, connect.NewError(connect.CodeUnauthenticated, errors.New("HTTP status 401 Unauthorized")), false},
		{"ephemeral rejected on declare", true, false, errUnregisteredRunner, nil, true},
		{"persistent rejected on declare", false, false, errUnregisteredRunner, nil, false},
		{"ephemeral stopped during declare outage", true, true, connect.NewError(connect.CodeUnavailable, errors.New("bad gateway")), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			service := &daemonTestService{declareErr: tc.declareErr, fetchErr: tc.fetchErr, beforeTask: tc.beforeTask, cancel: cancel}
			_, handler := runnerv1connect.NewRunnerServiceHandler(service)
			server := httptest.NewServer(http.StripPrefix("/api/actions", handler))
			defer server.Close()
			dir := t.TempDir()
			regFile := filepath.Join(dir, "registration.json")
			configFile := filepath.Join(dir, "config.yaml")
			require.NoError(t, os.WriteFile(configFile, []byte("runner:\n  file: "+regFile+"\n  idle_cleanup_interval: 0s\ncache:\n  enabled: false\n"), 0o600))
			require.NoError(t, config.SaveRegistration(regFile, &config.Registration{Address: server.URL, UUID: "test", Name: "test", Token: "test", Labels: []string{"host:host"}, Ephemeral: tc.ephemeral}))
			err := runDaemon(ctx, &daemonArgs{Once: true}, &configFile)(nil, nil)
			if tc.declareErr == nil && tc.fetchErr == nil || tc.beforeTask {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, int32(2), service.declareCalls.Load())
			if tc.removed {
				require.NoFileExists(t, regFile)
			} else {
				require.FileExists(t, regFile)
			}
		})
	}
}

func TestShouldRetryDeclare(t *testing.T) {
	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{connect.NewError(connect.CodeUnavailable, errors.New("bad gateway")), true},
		{connect.NewWireError(connect.CodeInternal, errors.New("update runner")), true},
		{connect.NewError(connect.CodeUnavailable, &tls.CertificateVerificationError{Err: errors.New("unknown authority")}), false},
		{connect.NewError(connect.CodeUnavailable, http.ErrSchemeMismatch), false},
		{connect.NewError(connect.CodeUnavailable, tls.AlertError(42)), false},
		{connect.NewError(connect.CodePermissionDenied, errors.New("forbidden")), false},
		{connect.NewWireError(connect.CodeUnknown, errors.New("unregistered runner")), false},
	} {
		require.Equal(t, tc.retry, shouldRetryDeclare(tc.err), tc.err.Error())
	}
}
