// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package report

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/runner"
	"gitea.com/gitea/runner/internal/pkg/client/mocks"
	"gitea.com/gitea/runner/internal/pkg/config"
	"gitea.com/gitea/runner/internal/pkg/metrics"

	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReporter_CompositeStepResultDoesNotFinishStep(t *testing.T) {
	reporter := &Reporter{state: &runnerv1.TaskState{}, logReplacer: strings.NewReplacer()}
	reporter.ResetSteps(newTestSteps(1))
	data := map[string]any{"stage": runner.StageMain, "stepNumber": 0, "stepID": []string{"0", "0"}, "raw_output": true, "stepResult": model.StepStatusFailure}

	require.NoError(t, fire(reporter, logEntry{Data: data}))
	assert.Equal(t, runnerv1.Result_RESULT_UNSPECIFIED, reporter.state.Steps[0].Result)

	delete(data, "stepID")
	data["stepResult"] = model.StepStatusSuccess
	require.NoError(t, fire(reporter, logEntry{Data: data}))
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, reporter.state.Steps[0].Result)
	assert.Equal(t, int64(2), reporter.state.Steps[0].LogLength)
}

func TestReporter_LogLevelFiltering(t *testing.T) {
	origLevel := logging.GetLevel()
	logging.SetLevel(logging.LevelInfo)
	defer logging.SetLevel(origLevel)

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
		return &runnerv1.UpdateLogResponse{
			AckIndex: req.Index + int64(len(req.Rows)),
		}, nil
	})
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(func(_ context.Context, req *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
		return &runnerv1.UpdateTaskResponse{}, nil
	})

	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportLevel = "warn"
	reporter := newTestReporter(t, client, cfg)
	assert.False(t, reporter.shouldAppendLogRow(logging.Record{Level: logging.LevelInfo}))
	assert.True(t, reporter.shouldAppendLogRow(logging.Record{Level: logging.LevelWarn}))
	reporter.RunDaemon()
	defer func() {
		require.NoError(t, reporter.Close(""))
	}()
	reporter.ResetSteps(newTestSteps(2))

	dataStep0 := map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true}
	dataStep0Internal := map[string]any{"stage": "Main", "stepNumber": 0}

	require.NoError(t, fire(reporter, logEntry{Message: "step output", Data: dataStep0, Level: logging.LevelInfo}))
	require.NoError(t, fire(reporter, logEntry{Message: "step debug output", Data: dataStep0, Level: logging.LevelDebug}))
	assert.Equal(t, int64(2), reporter.state.Steps[0].LogLength, "raw_output entries must always be forwarded")

	require.NoError(t, fire(reporter, logEntry{Message: "internal info", Data: dataStep0Internal, Level: logging.LevelInfo}))
	require.NoError(t, fire(reporter, logEntry{Message: "internal debug", Data: dataStep0Internal, Level: logging.LevelDebug}))

	require.NoError(t, fire(reporter, logEntry{
		Message: "Skipping step",
		Data: map[string]any{
			"stage":      "Main",
			"stepNumber": 1,
			"stepResult": "skipped",
		},
		Level: logging.LevelDebug,
	}))
	assert.Equal(t, runnerv1.Result_RESULT_SKIPPED, reporter.state.Steps[1].Result,
		"stepResult at DebugLevel must update step state even when log entry is filtered from job log output")
}

func TestReporter_EphemeralRunnerDeletion(t *testing.T) {
	runnerDeleted := false

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			if runnerDeleted {
				return nil, errors.New("runner has been deleted")
			}
			return &runnerv1.UpdateLogResponse{
				AckIndex: req.Index + int64(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, req *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			if req.State != nil && req.State.Result != runnerv1.Result_RESULT_UNSPECIFIED {
				runnerDeleted = true
			}
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	cfg, _ := config.LoadDefault("")
	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(1))

	require.NoError(t, fire(reporter, logEntry{
		Message: "build output",
		Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
	}))

	require.NoError(t, reporter.ReportLog(false))

	reporter.stateMu.Lock()
	reporter.closed = true
	for _, v := range reporter.state.Steps {
		if v.Result == runnerv1.Result_RESULT_UNSPECIFIED {
			v.Result = runnerv1.Result_RESULT_CANCELLED
		}
	}
	reporter.state.Result = runnerv1.Result_RESULT_FAILURE
	reporter.logRows = append(reporter.logRows, &runnerv1.LogRow{
		Time:    timestamppb.Now(),
		Content: "Early termination",
	})
	reporter.state.StoppedAt = timestamppb.Now()
	reporter.stateMu.Unlock()

	require.NoError(t, reporter.ReportState(false))
	assert.False(t, runnerDeleted, "runner must not be deleted by RunDaemon's ReportState")

	require.NoError(t, reporter.ReportLog(false))
	close(reporter.daemon)
	require.NoError(t, reporter.ReportLog(true), "final log upload must not fail: runner should not be deleted before Close finishes sending logs")
	require.NoError(t, reporter.ReportState(true), "final state update should work: runner should not be deleted before Close finishes sending logs")
}

func TestReporter_RunDaemonClose_Race(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			return &runnerv1.UpdateLogResponse{
				AckIndex: req.Index + int64(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	ctx, cancel := context.WithCancel(t.Context())
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{
		Context: taskCtx,
	}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	reporter.RunDaemon()

	require.NoError(t, reporter.Close(""))

	cancel()
}

func TestReporter_LogFlush(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxLatency time.Duration
		batchSize  int
		messages   []string
	}{
		{"max latency timer", 100 * time.Millisecond, 1000, []string{"single log line"}},
		{"batch size", 10 * time.Second, 5, []string{"log line 0", "log line 1", "log line 2", "log line 3", "log line 4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var updateLogCalls atomic.Int64
			client := mocks.NewClient(t)
			client.On("UpdateLog", mock.Anything, mock.Anything).Return(
				func(_ctx context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
					updateLogCalls.Add(1)
					return &runnerv1.UpdateLogResponse{AckIndex: req.Index + int64(len(req.Rows))}, nil
				},
			)
			client.On("UpdateTask", mock.Anything, mock.Anything).Maybe().Return(&runnerv1.UpdateTaskResponse{}, nil)

			cfg, _ := config.LoadDefault("")
			cfg.Runner.LogReportInterval = 10 * time.Second
			cfg.Runner.LogReportMaxLatency = tc.maxLatency
			cfg.Runner.LogReportBatchSize = tc.batchSize
			reporter := newTestReporter(t, client, cfg)
			reporter.ResetSteps(newTestSteps(1))
			reporter.RunDaemon()
			defer func() { require.NoError(t, reporter.Close("")) }()

			for _, message := range tc.messages {
				require.NoError(t, fire(reporter, logEntry{
					Message: message,
					Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
				}))
			}
			assert.Eventually(t, func() bool {
				return updateLogCalls.Load() > 0
			}, 500*time.Millisecond, 10*time.Millisecond, "logs must flush before the periodic timers")
		})
	}
}

func TestReporter_StateChangedNotLostDuringReport(t *testing.T) {
	var updateTaskCalls atomic.Int64
	inFlight := make(chan struct{})
	release := make(chan struct{})

	client := mocks.NewClient(t)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			n := updateTaskCalls.Add(1)
			if n == 1 {
				close(inFlight)
				<-release
			}
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	cfg, _ := config.LoadDefault("")
	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(2))

	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.stateMu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- reporter.ReportState(false)
	}()

	<-inFlight

	require.NoError(t, fire(reporter, logEntry{
		Message: "step starts",
		Data:    map[string]any{"stage": "Main", "stepNumber": 1, "raw_output": true},
	}))

	close(release)
	require.NoError(t, <-done)

	reporter.stateMu.RLock()
	changed := reporter.stateChanged
	reporter.stateMu.RUnlock()
	assert.True(t, changed, "stateChanged must remain true after a concurrent log record during in-flight ReportState")

	require.NoError(t, reporter.ReportState(false))
	assert.Equal(t, int64(2), updateTaskCalls.Load(), "concurrent log record change must trigger a second UpdateTask, not be silently lost")
}

func TestReporter_StateChangedRestoredOnError(t *testing.T) {
	var updateTaskCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			n := updateTaskCalls.Add(1)
			if n == 1 {
				return nil, errors.New("transient network error")
			}
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	cfg, _ := config.LoadDefault("")
	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(1))

	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.stateMu.Unlock()

	require.Error(t, reporter.ReportState(false))

	reporter.stateMu.RLock()
	changed := reporter.stateChanged
	reporter.stateMu.RUnlock()
	assert.True(t, changed, "stateChanged must be restored to true after UpdateTask error so the change is retried")

	require.NoError(t, reporter.ReportState(false))
	assert.Equal(t, int64(2), updateTaskCalls.Load())
}

func TestReporter_StateNotifyFlush(t *testing.T) {
	var updateTaskCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			return &runnerv1.UpdateLogResponse{
				AckIndex: req.Index + int64(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			updateTaskCalls.Add(1)
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	cfg, _ := config.LoadDefault("")
	cfg.Runner.StateReportInterval = 10 * time.Second
	cfg.Runner.LogReportInterval = 10 * time.Second

	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()
	defer func() {
		_ = reporter.Close("")
	}()

	require.NoError(t, fire(reporter, logEntry{
		Message: "step starting",
		Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
	}))

	assert.Eventually(t, func() bool {
		return updateTaskCalls.Load() > 0
	}, 500*time.Millisecond, 10*time.Millisecond,
		"step transition should have triggered immediate state flush via stateNotify")
}

func TestReporter_CloseAlwaysSendsRowsWithNoMore(t *testing.T) {
	var lastReq atomic.Pointer[runnerv1.UpdateLogRequest]
	var noMoreCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			lastReq.Store(req)
			if req.NoMore {
				noMoreCalls.Add(1)
			}
			return &runnerv1.UpdateLogResponse{
				AckIndex: req.Index + int64(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportInterval = 10 * time.Second
	cfg.Runner.LogReportMaxLatency = 10 * time.Second
	cfg.Runner.StateReportInterval = 10 * time.Second
	cfg.Runner.LogReportBatchSize = 1000

	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()

	reporter.stateMu.Lock()
	reporter.logOffset = 5
	reporter.state.Result = runnerv1.Result_RESULT_SUCCESS
	reporter.state.Steps[0].Result = runnerv1.Result_RESULT_SUCCESS
	reporter.state.StoppedAt = timestamppb.Now()
	reporter.stateMu.Unlock()

	require.NoError(t, reporter.Close(""))

	require.Equal(t, int64(1), noMoreCalls.Load(), "Close must send exactly one UpdateLog with NoMore=true")
	final := lastReq.Load()
	require.NotNil(t, final)
	assert.True(t, final.NoMore, "final UpdateLog must carry NoMore=true")
	assert.NotEmpty(t, final.Rows, "final UpdateLog must carry at least one row")
}

func TestReporter_StateHeartbeat(t *testing.T) {
	var updateTaskCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			updateTaskCalls.Add(1)
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	cfg, _ := config.LoadDefault("")
	cfg.Runner.StateReportInterval = 50 * time.Millisecond
	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(1))

	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.stateMu.Unlock()
	require.NoError(t, reporter.ReportState(false))
	require.Equal(t, int64(1), updateTaskCalls.Load())

	require.NoError(t, reporter.ReportState(false))
	assert.Equal(t, int64(1), updateTaskCalls.Load(), "no-op ReportState within stateReportInterval must skip")

	time.Sleep(2 * cfg.Runner.StateReportInterval)
	require.NoError(t, reporter.ReportState(false))
	assert.Equal(t, int64(2), updateTaskCalls.Load(), "ReportState must heartbeat after stateReportInterval even with no state change")
}

func TestReporter_ServerCancelStillFlushesFinal(t *testing.T) {
	var (
		updateTaskCalls    atomic.Int64
		finalLogNoMoreSeen atomic.Bool
		finalTaskStateSeen atomic.Bool
	)

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			if req.NoMore {
				finalLogNoMoreSeen.Store(true)
			}
			return &runnerv1.UpdateLogResponse{
				AckIndex: req.Index + int64(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			n := updateTaskCalls.Add(1)
			if n == 1 {
				return &runnerv1.UpdateTaskResponse{
					State: &runnerv1.TaskState{
						Result: runnerv1.Result_RESULT_CANCELLED,
					},
				}, nil
			}
			if req.State != nil && req.State.Result != runnerv1.Result_RESULT_UNSPECIFIED {
				finalTaskStateSeen.Store(true)
			}
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.stateMu.Unlock()

	require.NoError(t, reporter.ReportState(false))
	require.Equal(t, int64(1), updateTaskCalls.Load())

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected r.ctx to be cancelled after server returned RESULT_CANCELLED")
	}

	close(reporter.daemon)

	require.NoError(t, reporter.Close("cancelled"))

	assert.True(t, finalLogNoMoreSeen.Load(), "Close() must send a final UpdateLog{NoMore:true} even after server-side cancellation")
	assert.True(t, finalTaskStateSeen.Load(), "Close() must send a final UpdateTask with the populated final state even after server-side cancellation")
}

func TestReporter_CloseReportsCancelledOnCanceledCtx(t *testing.T) {
	var finalState atomic.Pointer[runnerv1.TaskState]
	var finalLogRows atomic.Pointer[[]*runnerv1.LogRow]

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			if req.NoMore {
				rows := append([]*runnerv1.LogRow(nil), req.Rows...)
				finalLogRows.Store(&rows)
			}
			return &runnerv1.UpdateLogResponse{
				AckIndex: req.Index + int64(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			if req.State != nil && req.State.Result != runnerv1.Result_RESULT_UNSPECIFIED {
				finalState.Store(req.State)
			}
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	ctx, cancel := context.WithCancel(t.Context())
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	cancel()

	close(reporter.daemon)

	require.NoError(t, reporter.Close(""))

	got := finalState.Load()
	require.NotNil(t, got, "Close() must send a final UpdateTask")
	assert.Equal(t, runnerv1.Result_RESULT_CANCELLED, got.Result,
		"final Result must be RESULT_CANCELLED when r.ctx is cancelled, not RESULT_FAILURE")
	require.Len(t, got.Steps, 1)
	assert.Equal(t, runnerv1.Result_RESULT_CANCELLED, got.Steps[0].Result,
		"unfinished steps must be marked RESULT_CANCELLED")

	rows := finalLogRows.Load()
	require.NotNil(t, rows, "Close() must send a final UpdateLog{NoMore:true}")
	var foundCancelled, foundEarlyTermination bool
	for _, r := range *rows {
		if r.Content == "Cancelled" {
			foundCancelled = true
		}
		if r.Content == "Early termination" {
			foundEarlyTermination = true
		}
	}
	assert.True(t, foundCancelled, "final log must contain a 'Cancelled' row")
	assert.False(t, foundEarlyTermination, "final log must not contain 'Early termination' on the cancel path")
}

func TestReporter_StopHeartbeats(t *testing.T) {
	var updateTaskCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			return &runnerv1.UpdateLogResponse{
				AckIndex: req.Index + int64(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			updateTaskCalls.Add(1)
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	cfg.Runner.StateReportInterval = 20 * time.Millisecond
	cfg.Runner.LogReportInterval = time.Hour

	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()

	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.state.Result = runnerv1.Result_RESULT_SUCCESS
	reporter.state.StoppedAt = timestamppb.Now()
	reporter.stateMu.Unlock()

	require.Eventually(t, func() bool {
		return updateTaskCalls.Load() >= 1
	}, time.Second, 5*time.Millisecond, "daemon must send at least one UpdateTask before StopHeartbeats")

	beforeStop := updateTaskCalls.Load()
	reporter.StopHeartbeats()

	select {
	case <-reporter.daemon:
	case <-time.After(time.Second):
		t.Fatal("StopHeartbeats must stop the daemon loop")
	}

	time.Sleep(3 * cfg.Runner.StateReportInterval)
	assert.Equal(t, beforeStop, updateTaskCalls.Load(),
		"UpdateTask must not be called after StopHeartbeats")

	require.NoError(t, reporter.Close(""))
	assert.Greater(t, updateTaskCalls.Load(), beforeStop,
		"Close() must still send a final UpdateTask after StopHeartbeats")
}

func TestAppendLogRow(t *testing.T) {
	r := &Reporter{logReplacer: strings.NewReplacer("supersecret", "***")}
	r.appendLogRow(&runnerv1.LogRow{Time: timestamppb.Now(), Content: "hello supersecret"})
	require.Len(t, r.logRows, 1)
	assert.Equal(t, "hello ***", r.logRows[0].Content)

	r = &Reporter{logReplacer: strings.NewReplacer("a?b", "***")}
	r.appendLogRow(&runnerv1.LogRow{Time: timestamppb.Now(), Content: "a\xffb"})
	assert.Equal(t, "***", r.logRows[0].Content)

	assert.Equal(t, "::debug::d%0Anext", logRow(logging.Record{Message: "::debug::d%0Anext\r\n"}).Content)
}

func TestReporter_Enabled(t *testing.T) {
	assert.True(t, (&Reporter{}).Enabled(t.Context(), logging.LevelTrace))
}

func TestReporter_Result(t *testing.T) {
	r := &Reporter{state: &runnerv1.TaskState{Result: runnerv1.Result_RESULT_SUCCESS}}
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, r.Result())
}

func TestReporter_SetOutputs(t *testing.T) {
	r := &Reporter{state: &runnerv1.TaskState{}, logReplacer: strings.NewReplacer("s3cr3t", "***")}

	r.SetOutputs(map[string]string{"foo": "bar"})
	got, ok := r.outputs["foo"]
	require.True(t, ok)
	assert.Equal(t, "bar", got.value)

	r.SetOutputs(map[string]string{"foo": "baz"})
	assert.Equal(t, "bar", r.outputs["foo"].value)

	longKey := strings.Repeat("k", maxOutputKeyLen+1)
	r.SetOutputs(map[string]string{longKey: "v"})
	_, ok = r.outputs[longKey]
	assert.False(t, ok)

	longValue := strings.Repeat("v", maxOutputValueLen+1)
	r.SetOutputs(map[string]string{"big": longValue})
	_, ok = r.outputs["big"]
	assert.False(t, ok)

	r.SetOutputs(map[string]string{"leaky": "has s3cr3t in it"})
	_, ok = r.outputs["leaky"]
	assert.False(t, ok)

	r.SetOutputs(map[string]string{"binary": "caf\xff"})
	got, ok = r.outputs["binary"]
	require.True(t, ok)
	assert.Equal(t, "caf\xff", got.value)

	maxValue := strings.Repeat("v", maxOutputValueLen)
	r.SetOutputs(map[string]string{"atlimit": maxValue})
	got, ok = r.outputs["atlimit"]
	require.True(t, ok)
	assert.Len(t, got.value, maxOutputValueLen)
}

func TestReporter_DebugSettingsAreNotMasked(t *testing.T) {
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	reporter := NewReporter(t.Context(), nil, nil, &runnerv1.Task{
		Context: taskCtx,
		Secrets: map[string]string{"ACTIONS_STEP_DEBUG": "true", "ACTIONS_RUNNER_DEBUG": "true", "TOKEN": "s3cr3t"},
	}, &config.Config{})
	defer deregisterGlobalMasks(reporter)

	assert.Equal(t, "debug is true", reporter.mask("debug is true"))
	assert.Equal(t, "***", reporter.mask("s3cr3t"))

	reporter.SetOutputs(map[string]string{"changed": "true"})
	assert.Equal(t, "true", reporter.outputs["changed"].value)
}

func TestReporter_OutputsSentOnce(t *testing.T) {
	client := mocks.NewClient(t)
	var reported []map[string]string
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			reported = append(reported, req.Outputs)
			return &runnerv1.UpdateTaskResponse{SentOutputs: slices.Collect(maps.Keys(req.Outputs))}, nil
		})

	cfg, _ := config.LoadDefault("")
	r := newTestReporter(t, client, cfg)

	r.SetOutputs(map[string]string{"foo": "bar"})
	require.NoError(t, r.ReportState(false))
	assert.True(t, r.outputs["foo"].sent)

	require.NoError(t, r.ReportState(true))
	require.Len(t, reported, 2)
	assert.Equal(t, map[string]string{"foo": "bar"}, reported[0])
	assert.Empty(t, reported[1])
}

func TestReporter_EffectiveCloseTimeout(t *testing.T) {
	assert.Equal(t, 10*time.Second, (&Reporter{}).effectiveCloseTimeout())
	assert.Equal(t, 5*time.Second, (&Reporter{closeTimeout: 5 * time.Second}).effectiveCloseTimeout())
}

func TestReporter_FlushFinalRetriesUntilSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		require.NoError(t, (&Reporter{}).flushFinal(func() error {
			if calls++; calls < 3 {
				return errors.New("unavailable")
			}
			return nil
		}))
		assert.Equal(t, 3, calls)
	})
}

func TestReporter_ParseResult(t *testing.T) {
	tests := []struct {
		name   string
		input  any
		want   runnerv1.Result
		wantOk bool
	}{
		{"job result string", "success", runnerv1.Result_RESULT_SUCCESS, true},
		{"failure string", "failure", runnerv1.Result_RESULT_FAILURE, true},
		{"step result stringer", runnerv1.Result_RESULT_SKIPPED, runnerv1.Result_RESULT_UNSPECIFIED, false},
		{"unknown string", "bogus", runnerv1.Result_RESULT_UNSPECIFIED, false},
		{"unsupported type", 123, runnerv1.Result_RESULT_UNSPECIFIED, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseResult(tt.input)
			assert.Equal(t, tt.wantOk, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReporter_masksEncodedSecrets(t *testing.T) {
	secret := `p@ss w"rd/1`
	r := &Reporter{logReplacer: strings.NewReplacer()}
	r.oldnew = runner.AppendSecretMasker(r.oldnew, secret)
	r.logReplacer = strings.NewReplacer(r.oldnew...)

	for _, line := range []string{
		"token: " + secret,
		"basic " + base64.StdEncoding.EncodeToString([]byte(secret)),
		"https://example.com/?token=" + url.QueryEscape(secret),
	} {
		masked := r.mask(line)
		assert.Contains(t, masked, "***")
		assert.NotContains(t, masked, secret)
		assert.NotContains(t, masked, base64.StdEncoding.EncodeToString([]byte(secret)))
	}
}

func TestReporter_AckIndexBeyondBuffer(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, _ *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
		return &runnerv1.UpdateLogResponse{AckIndex: 1000}, nil
	})

	cfg, _ := config.LoadDefault("")
	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(1))

	require.NoError(t, fire(reporter, logEntry{
		Message: "hello",
		Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
		Level:   logging.LevelInfo,
	}))

	require.NoError(t, reporter.ReportLog(false))
}

func TestReporter_CloseWithStuckDaemon(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return &runnerv1.UpdateLogResponse{
			AckIndex: req.Index + int64(len(req.Rows)),
		}, nil
	})
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(func(_ context.Context, _ *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
		return &runnerv1.UpdateTaskResponse{}, nil
	}).Maybe()

	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportInterval = 10 * time.Millisecond
	reporter := newTestReporter(t, client, cfg)
	reporter.daemonWait = time.Millisecond
	reporter.RunDaemon()
	reporter.ResetSteps(newTestSteps(1))

	require.NoError(t, fire(reporter, logEntry{
		Message: "hello",
		Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
		Level:   logging.LevelInfo,
	}))

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon never reached UpdateLog")
	}
	go func() {
		time.Sleep(10 * reporter.daemonWait)
		close(release)
	}()

	require.NoError(t, reporter.Close(""))
}

func TestReporter_ServerFailureResultIsReportedAsFailure(t *testing.T) {
	var lastState *runnerv1.TaskState
	client := mocks.NewClient(t)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(func(_ context.Context, req *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
		lastState = req.State
		return &runnerv1.UpdateTaskResponse{
			State: &runnerv1.TaskState{Result: runnerv1.Result_RESULT_FAILURE},
		}, nil
	})
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
		return &runnerv1.UpdateLogResponse{
			AckIndex: req.Index + int64(len(req.Rows)),
		}, nil
	}).Maybe()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	close(reporter.daemon)

	require.NoError(t, reporter.ReportState(false))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.NoError(t, reporter.Close(""))

	require.NotNil(t, lastState)
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, lastState.Result)
}

func TestReporter_NoteReport(t *testing.T) {
	hook := logtest.SetDefault(t)

	reporter := &Reporter{reportFailing: map[string]bool{}}

	reporter.noteReport(metrics.LabelMethodUpdateTask, fmt.Errorf("wrapped: %w", errOutputsNotSent))
	assert.Empty(t, hook.AllRecords())

	reporter.noteReport(metrics.LabelMethodUpdateTask, errors.New("connection refused"))
	reporter.noteReport(metrics.LabelMethodUpdateTask, errors.New("connection refused"))
	require.Len(t, hook.AllRecords(), 1)
	assert.Contains(t, hook.LastRecord().Message, "connection refused")

	reporter.noteReport(metrics.LabelMethodUpdateTask, nil)
	require.Len(t, hook.AllRecords(), 2)
	assert.Contains(t, hook.LastRecord().Message, "reconnected")
}

func TestReporter_CloseMasksLastWords(t *testing.T) {
	const secret = "supersecret"
	var rows []*runnerv1.LogRow

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			rows = append(rows, req.Rows...)
			return &runnerv1.UpdateLogResponse{
				AckIndex: req.Index + int64(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const idToken = "id-token-request-secret"
	taskCtx, err := structpb.NewStruct(map[string]any{"actions_id_token_request_token": idToken})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{
		Context: taskCtx,
		Secrets: map[string]string{"TOKEN": secret},
	}, cfg)
	close(reporter.daemon)

	require.NoError(t, reporter.Close("could not get job matrix: "+secret+" "+idToken))

	require.Len(t, rows, 1)
	assert.Equal(t, "could not get job matrix: *** ***", rows[0].Content)
}

type giteaLogModel struct {
	mu      sync.Mutex
	rows    int64
	claimed int64
}

func (m *giteaLogModel) appendRows(n int) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows += int64(n)
	return m.rows
}

func (m *giteaLogModel) applyState(state *runnerv1.TaskState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range state.GetSteps() {
		m.claimed = max(m.claimed, s.LogIndex+s.LogLength)
	}
}

func (m *giteaLogModel) snapshot() (rows, claimed int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows, m.claimed
}

func TestReporter_StepRangeCoversAckedRows(t *testing.T) {
	server := &giteaLogModel{}

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			return &runnerv1.UpdateLogResponse{
				AckIndex: server.appendRows(len(req.Rows)),
			}, nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error) {
			server.applyState(req.State)
			return &runnerv1.UpdateTaskResponse{}, nil
		},
	)

	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportInterval = time.Hour
	cfg.Runner.LogReportMaxLatency = time.Hour
	cfg.Runner.StateReportInterval = time.Hour
	cfg.Runner.LogReportBatchSize = 5

	reporter := newTestReporter(t, client, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()
	defer func() {
		_ = reporter.Close("")
	}()

	stepData := map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true}

	require.NoError(t, fire(reporter, logEntry{Message: "step starting", Data: stepData}))
	require.Eventually(t, func() bool {
		rows, claimed := server.snapshot()
		return rows == 1 && claimed == rows
	}, time.Second, 10*time.Millisecond, "the step transition should have flushed both log and state")

	for i := range cfg.Runner.LogReportBatchSize {
		require.NoError(t, fire(reporter, logEntry{
			Message: fmt.Sprintf("step output %d", i),
			Data:    stepData,
		}))
	}

	assert.Eventually(t, func() bool {
		rows, claimed := server.snapshot()
		return rows == 1+int64(cfg.Runner.LogReportBatchSize) && claimed == rows
	}, time.Second, 10*time.Millisecond,
		"every log row the server accepted must be covered by a reported step range")
}

func TestReporter_FlushWithoutAckedRowsSkipsState(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Once().Return(
		func(_ context.Context, _ *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			return &runnerv1.UpdateLogResponse{AckIndex: 0}, nil
		},
	)

	cfg, _ := config.LoadDefault("")
	reporter := newTestReporter(t, client, cfg)

	reporter.reportLogWithState()

	reporter.Logf("row")
	reporter.reportLogWithState()
}

func newTestReporter(t *testing.T, client *mocks.Client, cfg *config.Config) *Reporter {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	return NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
}

func newTestSteps(n int) []*model.Step {
	steps := make([]*model.Step, n)
	for i := range steps {
		steps[i] = &model.Step{Run: fmt.Sprintf("echo %d", i)}
	}
	return steps
}

func TestReporter_ReportedSteps(t *testing.T) {
	newReporter := func(t *testing.T, steps []*model.Step) (*Reporter, context.CancelFunc) {
		client := mocks.NewClient(t)
		client.On("UpdateLog", mock.Anything, mock.Anything).Maybe().Return(func(_ context.Context, req *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error) {
			return &runnerv1.UpdateLogResponse{AckIndex: req.Index + int64(len(req.Rows))}, nil
		})
		client.On("UpdateTask", mock.Anything, mock.Anything).Maybe().Return(&runnerv1.UpdateTaskResponse{}, nil)
		taskCtx, err := structpb.NewStruct(map[string]any{})
		require.NoError(t, err)
		cfg, _ := config.LoadDefault("")
		ctx, cancel := context.WithCancel(t.Context())
		reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx, Secrets: map[string]string{"TOKEN": "s3cr3t"}}, cfg)
		reporter.RunDaemon()
		reporter.Logf("runner version")
		reporter.ResetSteps(steps)
		return reporter, cancel
	}
	send := func(t *testing.T, reporter *Reporter, level logging.Level, msg string, data map[string]any) {
		t.Helper()
		require.NoError(t, fire(reporter, logEntry{Message: msg, Data: data, Level: level}))
	}
	type reported struct {
		stage            runnerv1.StepStage
		number           int64
		name             string
		result           runnerv1.Result
		logIndex, logLen int64
	}
	reportedOf := func(t *testing.T, reporter *Reporter) []reported {
		reporter.stateMu.RLock()
		defer reporter.stateMu.RUnlock()
		var ret []reported
		for i, v := range reporter.state.ReportedSteps {
			assert.EqualValues(t, i, v.Id)
			ret = append(ret, reported{v.Stage, v.Number, v.Name, v.Result, v.LogIndex, v.LogLength})
		}
		return ret
	}
	const (
		setup   = runnerv1.StepStage_STEP_STAGE_SETUP
		pre     = runnerv1.StepStage_STEP_STAGE_PRE
		main    = runnerv1.StepStage_STEP_STAGE_MAIN
		post    = runnerv1.StepStage_STEP_STAGE_POST
		cleanup = runnerv1.StepStage_STEP_STAGE_CLEANUP

		unspecified = runnerv1.Result_RESULT_UNSPECIFIED
		success     = runnerv1.Result_RESULT_SUCCESS
		failure     = runnerv1.Result_RESULT_FAILURE
		cancelled   = runnerv1.Result_RESULT_CANCELLED
		skipped     = runnerv1.Result_RESULT_SKIPPED
	)

	t.Run("steps as run", func(t *testing.T) {
		steps := newTestSteps(2)
		steps[0].Name = "Echo ${{ matrix.n }}"
		reporter, _ := newReporter(t, steps)
		reporter.Logf("workflow prepared")
		send(t, reporter, logging.LevelInfo, "Complete job name: test", map[string]any{"raw_output": true})
		send(t, reporter, logging.LevelDebug, "evaluating expression 'success()'", map[string]any{"stage": runner.StagePre, "stepNumber": 0})
		send(t, reporter, logging.LevelInfo, "Run Pre echo 0", map[string]any{"stage": runner.StagePre, "stepNumber": 0, "step": "Echo 1", runner.StepStartedField: true})
		send(t, reporter, logging.LevelInfo, "pre output", map[string]any{"stage": runner.StagePre, "stepNumber": 0, "raw_output": true})
		send(t, reporter, logging.LevelInfo, "Success - Pre echo 0", map[string]any{"stage": runner.StagePre, "stepNumber": 0, "stepResult": model.StepStatusSuccess})
		send(t, reporter, logging.LevelDebug, "evaluating expression 'false'", map[string]any{"stage": runner.StagePre, "stepNumber": 1})
		send(t, reporter, logging.LevelDebug, "Skipping step", map[string]any{"stage": runner.StagePre, "stepNumber": 1, "stepResult": model.StepStatusSkipped})
		send(t, reporter, logging.LevelInfo, "main output", map[string]any{"stage": runner.StageMain, "stepNumber": 0, "raw_output": true})
		send(t, reporter, logging.LevelInfo, "main output", map[string]any{"stage": runner.StageMain, "stepNumber": 0, "raw_output": true})
		send(t, reporter, logging.LevelInfo, "Success - Main echo 0", map[string]any{"stage": runner.StageMain, "stepNumber": 0, "stepResult": model.StepStatusSuccess})
		send(t, reporter, logging.LevelDebug, "Skipping step", map[string]any{"stage": runner.StageMain, "stepNumber": 1, "stepResult": model.StepStatusSkipped})
		send(t, reporter, logging.LevelInfo, "Run Post echo 0", map[string]any{"stage": runner.StagePost, "stepNumber": 0, runner.StepStartedField: true})
		send(t, reporter, logging.LevelInfo, "post output", map[string]any{"stage": runner.StagePost, "stepNumber": 0, "raw_output": true})
		send(t, reporter, logging.LevelInfo, "Success - Post echo 0", map[string]any{"stage": runner.StagePost, "stepNumber": 0, "stepResult": model.StepStatusSuccess})
		send(t, reporter, logging.LevelInfo, "Cleaning up container", map[string]any{"stage": runner.StageComplete})
		send(t, reporter, logging.LevelInfo, "Job succeeded", map[string]any{"stage": runner.StageComplete, "jobResult": model.StepStatusSuccess})

		want := []reported{
			{setup, 0, "Set up job", success, 0, 3},
			{pre, 0, "Pre Echo 1", success, 3, 3},
			{main, 0, "Echo 1", success, 6, 2},
			{main, 1, "Run echo 1", skipped, 0, 0},
			{post, 0, "Post Echo 1", success, 8, 3},
			{cleanup, 0, "Complete job", unspecified, 11, 2},
		}
		assert.Equal(t, want, reportedOf(t, reporter))

		send(t, reporter, logging.LevelInfo, "Removing images", map[string]any{"stage": runner.StageComplete})
		require.NoError(t, reporter.Close(""))
		want[5].result, want[5].logLen = success, 3
		assert.Equal(t, want, reportedOf(t, reporter))

		require.Len(t, reporter.state.Steps, 2)
		assert.EqualValues(t, 6, reporter.state.Steps[0].LogIndex)
		assert.EqualValues(t, 2, reporter.state.Steps[0].LogLength)
		assert.Equal(t, success, reporter.state.Steps[0].Result)
		assert.Equal(t, skipped, reporter.state.Steps[1].Result)
	})

	t.Run("failures and cancellation", func(t *testing.T) {
		reporter, cancel := newReporter(t, newTestSteps(3))
		started := func(stage string, n int) map[string]any {
			return map[string]any{"stage": stage, "stepNumber": n, runner.StepStartedField: true}
		}
		result := func(stage string, n int, result model.StepStatus, stepID ...string) map[string]any {
			fields := map[string]any{"stage": stage, "stepNumber": n, "stepResult": result}
			if len(stepID) > 0 {
				fields["stepID"] = stepID
			}
			return fields
		}
		send(t, reporter, logging.LevelInfo, "Run Pre echo 0", started(runner.StagePre, 0))
		send(t, reporter, logging.LevelInfo, "Success - Pre echo 0", result(runner.StagePre, 0, model.StepStatusSuccess))
		send(t, reporter, logging.LevelDebug, "Skipping step", result(runner.StagePre, 1, model.StepStatusSkipped))
		send(t, reporter, logging.LevelInfo, "Run Pre echo 2", started(runner.StagePre, 2))
		send(t, reporter, logging.LevelInfo, "Success - Pre echo 2", result(runner.StagePre, 2, model.StepStatusSuccess))
		send(t, reporter, logging.LevelInfo, "composite step", result(runner.StageMain, 0, model.StepStatusSuccess, "0", "inner"))
		send(t, reporter, logging.LevelInfo, "Failure - Main echo 0", result(runner.StageMain, 0, model.StepStatusFailure))
		send(t, reporter, logging.LevelInfo, "main output", map[string]any{"stage": runner.StageMain, "stepNumber": 1, "raw_output": true})
		send(t, reporter, logging.LevelInfo, "Run Post echo 0", started(runner.StagePost, 0))
		send(t, reporter, logging.LevelInfo, "Failure - Post echo 0", result(runner.StagePost, 0, model.StepStatusFailure))
		cancel()
		require.NoError(t, reporter.Close(""))

		results := make(map[string]runnerv1.Result)
		for _, v := range reportedOf(t, reporter) {
			results[v.name] = v.result
		}
		assert.Equal(t, map[string]runnerv1.Result{
			"Set up job":      success,
			"Pre Run echo 0":  success,
			"Pre Run echo 2":  success,
			"Run echo 0":      failure,
			"Run echo 1":      cancelled,
			"Run echo 2":      cancelled,
			"Post Run echo 0": failure,
			"Complete job":    cancelled,
		}, results)
		names := make([]string, 0, len(results))
		for _, v := range reportedOf(t, reporter) {
			names = append(names, v.name)
		}
		assert.Equal(t, []string{"Set up job", "Pre Run echo 0", "Pre Run echo 2", "Run echo 0", "Run echo 1", "Run echo 2", "Post Run echo 0", "Complete job"}, names)
	})

	t.Run("job failing during setup", func(t *testing.T) {
		reporter, _ := newReporter(t, newTestSteps(1))
		reporter.Logf("workflow prepared")
		send(t, reporter, logging.LevelInfo, "Job failed", map[string]any{"jobResult": model.StepStatusFailure})
		require.NoError(t, reporter.Close(""))

		assert.Equal(t, []reported{
			{setup, 0, "Set up job", failure, 0, 3},
			{main, 0, "Run echo 0", skipped, 0, 0},
			{cleanup, 0, "Complete job", skipped, 0, 0},
		}, reportedOf(t, reporter))
	})

	t.Run("action failing to download", func(t *testing.T) {
		reporter, _ := newReporter(t, newTestSteps(1))
		send(t, reporter, logging.LevelInfo, "Prepare all required actions", map[string]any{"raw_output": true})
		send(t, reporter, logging.LevelError, "##[error]unable to clone 'invalid-owner/invalid-action'", nil)
		send(t, reporter, logging.LevelInfo, "Cleaning up container", map[string]any{"stage": runner.StageComplete})
		send(t, reporter, logging.LevelInfo, "Job failed", map[string]any{"stage": runner.StageComplete, "jobResult": model.StepStatusFailure})
		require.NoError(t, reporter.Close(""))

		got := reportedOf(t, reporter)
		assert.Equal(t, []runnerv1.Result{failure, skipped, success}, []runnerv1.Result{got[0].result, got[1].result, got[2].result})
	})

	t.Run("cleanup failure gets a stop timestamp", func(t *testing.T) {
		reporter, _ := newReporter(t, newTestSteps(1))
		send(t, reporter, logging.LevelError, "##[error]job completed hook failed", map[string]any{"stage": runner.StageComplete})
		send(t, reporter, logging.LevelInfo, "Job failed", map[string]any{"stage": runner.StageComplete, "jobResult": model.StepStatusFailure})
		require.NoError(t, reporter.Close(""))

		step := reporter.state.ReportedSteps[len(reporter.state.ReportedSteps)-1]
		assert.Equal(t, failure, step.Result)
		require.NotNil(t, step.StartedAt)
		assert.NotNil(t, step.StoppedAt)
	})

	t.Run("step not reached in a succeeding job", func(t *testing.T) {
		reporter, _ := newReporter(t, newTestSteps(1))
		send(t, reporter, logging.LevelInfo, "Job succeeded", map[string]any{"stage": runner.StageComplete, "jobResult": model.StepStatusSuccess})
		require.NoError(t, reporter.Close(""))
		assert.Equal(t, skipped, reportedOf(t, reporter)[1].result)
	})

	t.Run("evaluated and masked names", func(t *testing.T) {
		steps := newTestSteps(2)
		steps[0].Run = "echo ${{ matrix.os }}\necho more"
		steps[1].Name = "Deploy with ${{ secrets.TOKEN }}"
		reporter, _ := newReporter(t, steps)
		send(t, reporter, logging.LevelDebug, "Run Main", map[string]any{"stage": runner.StageMain, "stepNumber": 0, "step": "echo linux\necho more"})
		send(t, reporter, logging.LevelDebug, "Run Main", map[string]any{"stage": runner.StageMain, "stepNumber": 1, "step": "Deploy with s3cr3t"})
		require.NoError(t, reporter.Close(""))

		names := []string{}
		for _, v := range reportedOf(t, reporter) {
			names = append(names, v.name)
		}
		assert.Equal(t, []string{"Set up job", "Run echo linux", "Deploy with ***", "Complete job"}, names)
	})

	t.Run("reusable workflow caller", func(t *testing.T) {
		reporter, _ := newReporter(t, nil)
		send(t, reporter, logging.LevelInfo, "Cleaning up container", map[string]any{"stage": runner.StageComplete})
		send(t, reporter, logging.LevelInfo, "Job succeeded", map[string]any{"stage": runner.StageComplete, "jobResult": model.StepStatusSuccess})
		require.NoError(t, reporter.Close(""))
		assert.Empty(t, reportedOf(t, reporter))
	})
}

type logEntry struct {
	Message string
	Data    map[string]any
	Level   logging.Level
}

func fire(r *Reporter, e logEntry) error {
	attrs := make([]logging.Attr, 0, len(e.Data))
	for k, v := range e.Data {
		attrs = append(attrs, logging.Attr{Key: k, Value: v})
	}
	return r.WithAttrs(attrs).Handle(context.Background(), logging.Record{Message: e.Message, Level: e.Level})
}
