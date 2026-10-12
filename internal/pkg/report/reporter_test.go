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
	"time"

	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/runner"
	"gitea.com/gitea/runner/internal/pkg/client/mocks"
	"gitea.com/gitea/runner/internal/pkg/config"
	"gitea.com/gitea/runner/internal/pkg/metrics"

	connect_go "connectrpc.com/connect"
	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReporter_parseLogRow(t *testing.T) {
	tests := []struct {
		name               string
		debugOutputEnabled bool
		args               []string
		want               []string
	}{
		{
			"No command", false,
			[]string{"Hello, world!"},
			[]string{"Hello, world!"},
		},
		{
			"Add-mask", false,
			[]string{
				"foo mysecret bar",
				"::add-mask::mysecret",
				"foo mysecret bar",
			},
			[]string{
				"foo mysecret bar",
				"<nil>",
				"foo *** bar",
			},
		},
		{
			"Add-mask-multiline", false,
			[]string{
				"foo mysecret bar",
				"::add-mask::LINE1%0ALINE2",
				"foo LINE1 bar",
			},
			[]string{
				"foo mysecret bar",
				"<nil>",
				"foo *** bar",
			},
		},
		{
			"Debug enabled", true,
			[]string{
				"::debug::GitHub Actions runtime token access controls",
				// Left escaped: the web UI decodes it, and a real newline would not survive storage.
				"::debug::first%0Asecond",
			},
			[]string{
				"::debug::GitHub Actions runtime token access controls",
				"::debug::first%0Asecond",
			},
		},
		{
			"Debug not enabled", false,
			[]string{
				"::debug::GitHub Actions runtime token access controls",
			},
			[]string{
				"<nil>",
			},
		},
		// The three annotation levels share one code path, so the property shapes are only
		// exercised under "error"; notice and warning just prove the level token round-trips.
		{
			"notice", false,
			[]string{
				"::notice::Gosh, that's not going to work",
			},
			[]string{
				"::notice::Gosh, that's not going to work",
			},
		},
		{
			"warning", false,
			[]string{
				"::warning::Gosh, that's not going to work",
			},
			[]string{
				"::warning::Gosh, that's not going to work",
			},
		},
		{
			"error", false,
			[]string{
				"::error file=file.name,line=42,endLine=48,title=Cool Title::Gosh, that's not going to work",
				"::error::Gosh, that's not going to work",
				"::error file=file.name,line=42,col=7::Gosh, that's not going to work",
				// The message keeps its own '::', the property list ends at the first one.
				"::error file=main.cpp,line=12::no member named 'foo' in 'std::vector<int>'",
				// GitHub matches property names case-insensitively.
				"::error File=file.name,Line=42,Col=7::Gosh, that's not going to work",
				// Only the property separators are decoded here, %25/%0A are left for the web UI.
				"::error file=a%3Ab.go,title=100%252C::still %25 escaped%0Aand multi-line",
			},
			[]string{
				"::error::file.name:42: Cool Title: Gosh, that's not going to work",
				"::error::Gosh, that's not going to work",
				"::error::file.name:42:7: Gosh, that's not going to work",
				"::error::main.cpp:12: no member named 'foo' in 'std::vector<int>'",
				"::error::file.name:42:7: Gosh, that's not going to work",
				"::error::a:b.go: 100%252C: still %25 escaped%0Aand multi-line",
			},
		},
		{
			"group", false,
			[]string{
				"::group::",
				"::endgroup::",
			},
			[]string{
				"::group::",
				"::endgroup::",
			},
		},
		{
			"stop-commands", false,
			[]string{
				"::add-mask::foo",
				"::stop-commands::myverycoolstoptoken",
				"::add-mask::bar",
				"::debug::Stuff",
				"myverycoolstoptoken",
				"::add-mask::baz",
				"::myverycoolstoptoken::",
				"::add-mask::wibble",
				"foo bar baz wibble",
			},
			[]string{
				"<nil>",
				"<nil>",
				"::add-mask::bar",
				"::debug::Stuff",
				"myverycoolstoptoken",
				"::add-mask::baz",
				"<nil>",
				"<nil>",
				"*** bar baz ***",
			},
		},
		{
			// a token naming a real command must still resume
			"stop-commands with a command-named token", false,
			[]string{
				"::stop-commands::add-mask",
				"::set-output name=x::suppressed",
				"::add-mask::",
				"::add-mask::masked",
				"masked",
			},
			[]string{
				"<nil>",
				"::set-output name=x::suppressed",
				"<nil>",
				"<nil>",
				"***",
			},
		},
		{
			"unknown command", false,
			[]string{
				"::set-mask::foo",
			},
			[]string{
				"::set-mask::foo",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reporter{
				logReplacer:        strings.NewReplacer(),
				debugOutputEnabled: tt.debugOutputEnabled,
			}
			for idx, arg := range tt.args {
				rv := r.parseLogRow(logging.Record{Message: arg})
				got := "<nil>"

				if rv != nil {
					got = r.mask(rv.Content)
				}

				assert.Equal(t, tt.want[idx], got)
			}
		})
	}
}

// Both add-mask forms must register the secret and drop their own row: the runner forwards
// the raw line, so failing to consume it writes the secret straight to the job log.
func TestReporter_parseLogRowAddMask(t *testing.T) {
	for _, line := range []string{"::add-mask::supersecret", "##[add-mask]supersecret"} {
		r := &Reporter{logReplacer: strings.NewReplacer()}

		assert.Nil(t, r.parseLogRow(logging.Record{Message: line}), line)

		assert.Equal(t, "using *** now", r.mask("using supersecret now"), line)
	}
}

func TestReporter_Handle(t *testing.T) {
	t.Run("ignore command lines", func(t *testing.T) {
		client := mocks.NewClient(t)
		client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			t.Logf("Received UpdateLog: %s", req.Msg.String())
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		})
		client.On("UpdateTask", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			t.Logf("Received UpdateTask: %s", req.Msg.String())
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		taskCtx, err := structpb.NewStruct(map[string]any{})
		require.NoError(t, err)
		cfg, _ := config.LoadDefault("")
		reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{
			Context: taskCtx,
		}, cfg)
		reporter.RunDaemon()
		defer func() {
			require.NoError(t, reporter.Close(""))
		}()
		reporter.ResetSteps(newTestSteps(5))

		dataStep0 := map[string]any{
			"stage":      "Main",
			"stepNumber": 0,
			"raw_output": true,
		}

		require.NoError(t, fire(reporter, logEntry{Message: "regular log line", Data: dataStep0}))
		require.NoError(t, fire(reporter, logEntry{Message: "::debug::debug log line", Data: dataStep0}))
		require.NoError(t, fire(reporter, logEntry{Message: "regular log line", Data: dataStep0}))
		require.NoError(t, fire(reporter, logEntry{Message: "::debug::debug log line", Data: dataStep0}))
		require.NoError(t, fire(reporter, logEntry{Message: "::debug::debug log line", Data: dataStep0}))
		require.NoError(t, fire(reporter, logEntry{Message: "regular log line", Data: dataStep0}))
		require.NoError(t, fire(reporter, logEntry{Message: "composite step result", Data: map[string]any{
			"stage":      "Main",
			"stepID":     []string{"0", "0"},
			"stepNumber": 0,
			"raw_output": true,
			"stepResult": model.StepStatusFailure,
		}}))
		assert.Equal(t, runnerv1.Result_RESULT_UNSPECIFIED, reporter.state.Steps[0].Result)
		require.NoError(t, fire(reporter, logEntry{Message: "step result", Data: map[string]any{
			"stage":      "Main",
			"stepNumber": 0,
			"raw_output": true,
			"stepResult": model.StepStatusSuccess,
		}}))
		assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, reporter.state.Steps[0].Result)

		assert.Equal(t, int64(5), reporter.state.Steps[0].LogLength)
	})
}

func TestReporter_LogLevelFiltering(t *testing.T) {
	// Set global level to Info so Debug entries should be filtered.
	origLevel := logging.GetLevel()
	logging.SetLevel(logging.LevelInfo)
	defer logging.SetLevel(origLevel)

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
		return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
			AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
		}), nil
	})
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
		return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.RunDaemon()
	defer func() {
		require.NoError(t, reporter.Close(""))
	}()
	reporter.ResetSteps(newTestSteps(2))

	dataStep0 := map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true}
	dataStep0Internal := map[string]any{"stage": "Main", "stepNumber": 0}

	// raw_output entries always appear in job log regardless of level.
	require.NoError(t, fire(reporter, logEntry{Message: "step output", Data: dataStep0, Level: logging.LevelInfo}))
	require.NoError(t, fire(reporter, logEntry{Message: "step debug output", Data: dataStep0, Level: logging.LevelDebug}))
	assert.Equal(t, int64(2), reporter.state.Steps[0].LogLength, "raw_output entries must always be forwarded")

	// Non-raw_output entries during steps are not added to logRows regardless of level.
	require.NoError(t, fire(reporter, logEntry{Message: "internal info", Data: dataStep0Internal, Level: logging.LevelInfo}))
	require.NoError(t, fire(reporter, logEntry{Message: "internal debug", Data: dataStep0Internal, Level: logging.LevelDebug}))

	// stepResult at DebugLevel (skipped step) must still update state even when filtered from log.
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

// TestReporter_EphemeralRunnerDeletion reproduces the exact scenario from
// https://gitea.com/gitea/runner/issues/793:
//
//  1. RunDaemon calls ReportLog(false) — runner is still alive
//  2. Close() updates state to Result=FAILURE (between RunDaemon's ReportLog and ReportState)
//  3. RunDaemon's ReportState() would clone the completed state and send it,
//     but the fix makes ReportState return early when closed, preventing this
//  4. Close's ReportLog(true) succeeds because the runner was not deleted
func TestReporter_EphemeralRunnerDeletion(t *testing.T) {
	runnerDeleted := false

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			if runnerDeleted {
				return nil, errors.New("runner has been deleted")
			}
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			// Server deletes ephemeral runner when it receives a completed state
			if req.Msg.State != nil && req.Msg.State.Result != runnerv1.Result_RESULT_UNSPECIFIED {
				runnerDeleted = true
			}
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	// Fire a log entry to create pending data
	require.NoError(t, fire(reporter, logEntry{
		Message: "build output",
		Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
	}))

	// Step 1: RunDaemon calls ReportLog(false) — runner is still alive
	require.NoError(t, reporter.ReportLog(false))

	// Step 2: Close() updates state — sets Result=FAILURE and marks steps cancelled.
	// In the real race, this happens while RunDaemon is between ReportLog and ReportState.
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

	// Step 3: RunDaemon's ReportState() — with the fix, this returns early
	// because closed=true, preventing the server from deleting the runner.
	require.NoError(t, reporter.ReportState(false))
	assert.False(t, runnerDeleted, "runner must not be deleted by RunDaemon's ReportState")

	// Step 4: Close's final log upload succeeds because the runner is still alive.
	// Flush pending rows first, then send the noMore signal (matching Close's retry behavior).
	require.NoError(t, reporter.ReportLog(false))
	// Acknowledge Close as done in daemon
	close(reporter.daemon)
	err = reporter.ReportLog(true)
	require.NoError(t, err, "final log upload must not fail: runner should not be deleted before Close finishes sending logs")
	err = reporter.ReportState(true)
	require.NoError(t, err, "final state update should work: runner should not be deleted before Close finishes sending logs")
}

func TestReporter_RunDaemonClose_Race(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{
		Context: taskCtx,
	}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	// Start the daemon loop — RunDaemon spawns a goroutine internally.
	reporter.RunDaemon()

	// Close concurrently — this races with the daemon goroutine on r.closed.
	require.NoError(t, reporter.Close(""))

	// Cancel context so the daemon goroutine exits cleanly.
	cancel()
}

// TestReporter_MaxLatencyTimer verifies that the maxLatencyTimer flushes a
// single buffered log row before the periodic logTicker fires.
//
// Setup: logReportInterval=10s (effectively never), maxLatency=100ms.
// Fire one log line, then assert UpdateLog is called within 500ms.
func TestReporter_MaxLatencyTimer(t *testing.T) {
	var updateLogCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			updateLogCalls.Add(1)
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	// Custom config: logTicker=10s (won't fire during test), maxLatency=100ms
	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportInterval = 10 * time.Second
	cfg.Runner.LogReportMaxLatency = 100 * time.Millisecond
	cfg.Runner.LogReportBatchSize = 1000 // won't trigger batch flush

	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()
	defer func() {
		_ = reporter.Close("")
	}()

	// Fire a single log line — not enough to trigger batch flush
	require.NoError(t, fire(reporter, logEntry{
		Message: "single log line",
		Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
	}))

	// maxLatencyTimer should flush within ~100ms. Wait up to 500ms.
	assert.Eventually(t, func() bool {
		return updateLogCalls.Load() > 0
	}, 500*time.Millisecond, 10*time.Millisecond,
		"maxLatencyTimer should have flushed the log before logTicker (10s)")
}

// TestReporter_BatchSizeFlush verifies that reaching logBatchSize triggers
// an immediate log flush without waiting for any timer.
func TestReporter_BatchSizeFlush(t *testing.T) {
	var updateLogCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			updateLogCalls.Add(1)
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	// Custom config: large timers, small batch size
	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportInterval = 10 * time.Second
	cfg.Runner.LogReportMaxLatency = 10 * time.Second
	cfg.Runner.LogReportBatchSize = 5

	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()
	defer func() {
		_ = reporter.Close("")
	}()

	// Fire exactly batchSize log lines
	for i := range 5 {
		require.NoError(t, fire(reporter, logEntry{
			Message: fmt.Sprintf("log line %d", i),
			Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
		}))
	}

	// Batch threshold should trigger immediate flush
	assert.Eventually(t, func() bool {
		return updateLogCalls.Load() > 0
	}, 500*time.Millisecond, 10*time.Millisecond,
		"batch size threshold should have triggered immediate flush")
}

// TestReporter_StateChangedNotLostDuringReport asserts that a log record arriving
// mid-UpdateTask re-dirties the flag so the change is picked up by the next report.
func TestReporter_StateChangedNotLostDuringReport(t *testing.T) {
	var updateTaskCalls atomic.Int64
	inFlight := make(chan struct{})
	release := make(chan struct{})

	client := mocks.NewClient(t)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			n := updateTaskCalls.Add(1)
			if n == 1 {
				// Signal that the first UpdateTask is in flight, then block until released.
				close(inFlight)
				<-release
			}
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(2))

	// Mark stateChanged=true so the first ReportState proceeds to UpdateTask.
	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.stateMu.Unlock()

	// Kick off the first ReportState in a goroutine — it will block in UpdateTask.
	done := make(chan error, 1)
	go func() {
		done <- reporter.ReportState(false)
	}()

	// Wait until UpdateTask is in flight (snapshot taken, flag consumed).
	<-inFlight

	// A concurrent log record modifies state — must re-flip stateChanged so the
	// change is not lost when the in-flight ReportState finishes.
	require.NoError(t, fire(reporter, logEntry{
		Message: "step starts",
		Data:    map[string]any{"stage": "Main", "stepNumber": 1, "raw_output": true},
	}))

	// Release the in-flight UpdateTask and wait for it to return.
	close(release)
	require.NoError(t, <-done)

	// stateChanged must still be true so the next ReportState picks up the
	// concurrent record's change instead of skipping via the early-return path.
	reporter.stateMu.RLock()
	changed := reporter.stateChanged
	reporter.stateMu.RUnlock()
	assert.True(t, changed, "stateChanged must remain true after a concurrent log record during in-flight ReportState")

	// And the next ReportState must actually send a second UpdateTask.
	require.NoError(t, reporter.ReportState(false))
	assert.Equal(t, int64(2), updateTaskCalls.Load(), "concurrent log record change must trigger a second UpdateTask, not be silently lost")
}

// TestReporter_StateChangedRestoredOnError verifies that when UpdateTask fails,
// the dirty flag is restored so the snapshotted change isn't silently lost.
func TestReporter_StateChangedRestoredOnError(t *testing.T) {
	var updateTaskCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			n := updateTaskCalls.Add(1)
			if n == 1 {
				return nil, errors.New("transient network error")
			}
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.stateMu.Unlock()

	// First ReportState fails — flag must be restored to true.
	require.Error(t, reporter.ReportState(false))

	reporter.stateMu.RLock()
	changed := reporter.stateChanged
	reporter.stateMu.RUnlock()
	assert.True(t, changed, "stateChanged must be restored to true after UpdateTask error so the change is retried")

	// The next ReportState should still issue a request because the flag was restored.
	require.NoError(t, reporter.ReportState(false))
	assert.Equal(t, int64(2), updateTaskCalls.Load())
}

// TestReporter_StateNotifyFlush verifies that step transitions trigger
// an immediate state flush via the stateNotify channel.
func TestReporter_StateNotifyFlush(t *testing.T) {
	var updateTaskCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			updateTaskCalls.Add(1)
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	// Custom config: large state interval so only stateNotify can trigger
	cfg, _ := config.LoadDefault("")
	cfg.Runner.StateReportInterval = 10 * time.Second
	cfg.Runner.LogReportInterval = 10 * time.Second

	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()
	defer func() {
		_ = reporter.Close("")
	}()

	// Fire a log entry that starts a step — this triggers stateNotify
	require.NoError(t, fire(reporter, logEntry{
		Message: "step starting",
		Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
	}))

	// stateNotify should trigger immediate UpdateTask call
	assert.Eventually(t, func() bool {
		return updateTaskCalls.Load() > 0
	}, 500*time.Millisecond, 10*time.Millisecond,
		"step transition should have triggered immediate state flush via stateNotify")
}

// Regression test for https://gitea.com/gitea/runner/issues/950: Close() must
// always send a final UpdateLog with NoMore=true carrying at least one row,
// otherwise the server's len(Rows)==0 short-circuit skips TransferLogs.
// TODO: Remove after https://github.com/go-gitea/gitea/pull/37631 is in all
// supported branches, e.g. v1.28+.
func TestReporter_CloseAlwaysSendsRowsWithNoMore(t *testing.T) {
	var lastReq atomic.Pointer[runnerv1.UpdateLogRequest]
	var noMoreCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			lastReq.Store(req.Msg)
			if req.Msg.NoMore {
				noMoreCalls.Add(1)
			}
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	// Intervals large enough that no daemon-driven flush fires during the test.
	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportInterval = 10 * time.Second
	cfg.Runner.LogReportMaxLatency = 10 * time.Second
	cfg.Runner.StateReportInterval = 10 * time.Second
	cfg.Runner.LogReportBatchSize = 1000

	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()

	// Simulate a successful job whose log buffer was already drained by the
	// daemon (logOffset > 0, logRows empty, terminal Result set). This is the
	// state Close() lands in for the typical successful job under #819.
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

// TestReporter_StateHeartbeat verifies that ReportState sends a heartbeat
// UpdateTask once stateReportInterval has elapsed since the last successful
// report, even when nothing has changed. Without this, long-running silent
// jobs (no log output, no step transitions) cause the server to time the
// task out and cancel it (#826).
func TestReporter_StateHeartbeat(t *testing.T) {
	var updateTaskCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			updateTaskCalls.Add(1)
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	cfg.Runner.StateReportInterval = 50 * time.Millisecond
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	// First call has no prior report — sends to seed lastReportedAt.
	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.stateMu.Unlock()
	require.NoError(t, reporter.ReportState(false))
	require.Equal(t, int64(1), updateTaskCalls.Load())

	// Second call immediately after with nothing changed — must skip.
	require.NoError(t, reporter.ReportState(false))
	assert.Equal(t, int64(1), updateTaskCalls.Load(), "no-op ReportState within stateReportInterval must skip")

	// After stateReportInterval elapses, a heartbeat must fire even with no changes.
	time.Sleep(2 * cfg.Runner.StateReportInterval)
	require.NoError(t, reporter.ReportState(false))
	assert.Equal(t, int64(2), updateTaskCalls.Load(), "ReportState must heartbeat after stateReportInterval even with no state change")
}

// TestReporter_ServerCancelStillFlushesFinal asserts that when the Gitea server
// returns RESULT_CANCELLED on an in-flight UpdateTask (which causes the
// reporter to cancel the task context), Close() still successfully sends the
// final UpdateLog{NoMore:true} and the final UpdateTask carrying the populated
// final state. Before the fix this final flush used r.ctx, which was just
// cancelled, so retry-go aborted on its context check and Gitea never received
// the runner's acknowledgement of the cancel.
func TestReporter_ServerCancelStillFlushesFinal(t *testing.T) {
	var (
		updateTaskCalls    atomic.Int64
		finalLogNoMoreSeen atomic.Bool
		finalTaskStateSeen atomic.Bool
	)

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			if req.Msg.NoMore {
				finalLogNoMoreSeen.Store(true)
			}
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	// The first UpdateTask returns RESULT_CANCELLED — modelling a server-side
	// cancellation; the reporter must call r.cancel() in response. The final
	// UpdateTask issued by Close() must still arrive even though r.ctx is now
	// cancelled.
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			n := updateTaskCalls.Add(1)
			if n == 1 {
				return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{
					State: &runnerv1.TaskState{
						Result: runnerv1.Result_RESULT_CANCELLED,
					},
				}), nil
			}
			if req.Msg.State != nil && req.Msg.State.Result != runnerv1.Result_RESULT_UNSPECIFIED {
				finalTaskStateSeen.Store(true)
			}
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	// Force the first ReportState to actually call UpdateTask.
	reporter.stateMu.Lock()
	reporter.stateChanged = true
	reporter.stateMu.Unlock()

	// First ReportState — server returns RESULT_CANCELLED, reporter cancels r.ctx.
	require.NoError(t, reporter.ReportState(false))
	require.Equal(t, int64(1), updateTaskCalls.Load())

	select {
	case <-ctx.Done():
		// Expected: reporter called cancel() because the server reported the task as cancelled.
	case <-time.After(time.Second):
		t.Fatal("expected r.ctx to be cancelled after server returned RESULT_CANCELLED")
	}

	// The test does not start the daemon goroutine; close(r.daemon) so Close()
	// proceeds without waiting on its 60s timeout.
	close(reporter.daemon)

	// Now Close() runs. Before the fix, both final RPCs aborted on the cancelled
	// r.ctx via retry.Context. After the fix, Close() uses a detached context and
	// the per-RPC rpcCtx() falls back to a fresh ctx, so both calls succeed.
	require.NoError(t, reporter.Close("cancelled"))

	assert.True(t, finalLogNoMoreSeen.Load(), "Close() must send a final UpdateLog{NoMore:true} even after server-side cancellation")
	assert.True(t, finalTaskStateSeen.Load(), "Close() must send a final UpdateTask with the populated final state even after server-side cancellation")
}

// TestReporter_CloseReportsCancelledOnCanceledCtx asserts that when Close()
// runs on a reporter whose state has not been finalised AND whose context has
// been cancelled, the synthesised final state carries RESULT_CANCELLED and
// the appended log row reads "Cancelled" — not RESULT_FAILURE / "Early
// termination". This is the runner-side half of the Running -> Cancelling ->
// Cancelled flow: it gives Gitea an explicit cancel acknowledgement rather
// than a generic failure when the job is torn down on the cancel path.
func TestReporter_CloseReportsCancelledOnCanceledCtx(t *testing.T) {
	var finalState atomic.Pointer[runnerv1.TaskState]
	var finalLogRows atomic.Pointer[[]*runnerv1.LogRow]

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			if req.Msg.NoMore {
				rows := append([]*runnerv1.LogRow(nil), req.Msg.Rows...)
				finalLogRows.Store(&rows)
			}
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			if req.Msg.State != nil && req.Msg.State.Result != runnerv1.Result_RESULT_UNSPECIFIED {
				finalState.Store(req.Msg.State)
			}
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	// Simulate the cancellation path: r.ctx is cancelled before Close() runs.
	cancel()

	// Skip the daemon wait inside Close().
	close(reporter.daemon)

	// Empty lastWords so Close() picks the synthesised value.
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

// TestReporter_StopHeartbeats verifies that StopHeartbeats ends periodic
// UpdateTask heartbeats while Close() still flushes the final state.
func TestReporter_StopHeartbeats(t *testing.T) {
	var updateTaskCalls atomic.Int64

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Maybe().Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			updateTaskCalls.Add(1)
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	cfg.Runner.StateReportInterval = 20 * time.Millisecond
	cfg.Runner.LogReportInterval = time.Hour

	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
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
	r.appendLogRow(nil)
	r.appendLogRow(&runnerv1.LogRow{Time: timestamppb.Now(), Content: "hello supersecret"})
	require.Len(t, r.logRows, 1)
	assert.Equal(t, "hello ***", r.logRows[0].Content)

	// repairing the invalid byte spells out the secret, so the repair has to come first
	r = &Reporter{logReplacer: strings.NewReplacer("a?b", "***")}
	r.appendLogRow(&runnerv1.LogRow{Time: timestamppb.Now(), Content: "a\xffb"})
	assert.Equal(t, "***", r.logRows[0].Content)
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

	// first value wins: a later write to the same key is ignored
	r.SetOutputs(map[string]string{"foo": "baz"})
	assert.Equal(t, "bar", r.outputs["foo"].value)

	// keys longer than maxOutputKeyLen are dropped
	longKey := strings.Repeat("k", maxOutputKeyLen+1)
	r.SetOutputs(map[string]string{longKey: "v"})
	_, ok = r.outputs[longKey]
	assert.False(t, ok)

	// values longer than maxOutputValueLen are dropped
	longValue := strings.Repeat("v", maxOutputValueLen+1)
	r.SetOutputs(map[string]string{"big": longValue})
	_, ok = r.outputs["big"]
	assert.False(t, ok)

	// a value carrying a secret is skipped, as GitHub does, rather than sent masked
	r.SetOutputs(map[string]string{"leaky": "has s3cr3t in it"})
	_, ok = r.outputs["leaky"]
	assert.False(t, ok)

	// invalid UTF-8 is not a secret, so the value is kept as it is rather than dropped
	r.SetOutputs(map[string]string{"binary": "caf\xff"})
	got, ok = r.outputs["binary"]
	require.True(t, ok)
	assert.Equal(t, "caf\xff", got.value)

	// a value at exactly the limit is still stored
	maxValue := strings.Repeat("v", maxOutputValueLen)
	r.SetOutputs(map[string]string{"atlimit": maxValue})
	got, ok = r.outputs["atlimit"]
	require.True(t, ok)
	assert.Len(t, got.value, maxOutputValueLen)
}

// Gitea delivers ACTIONS_STEP_DEBUG as a secret, so masking "true" would drop any job output
// saying it. GitHub skips the same two keys.
func TestReporter_DebugSettingsAreNotMasked(t *testing.T) {
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	reporter := NewReporter(context.Background(), nil, nil, &runnerv1.Task{
		Context: taskCtx,
		Secrets: map[string]string{"ACTIONS_STEP_DEBUG": "true", "ACTIONS_RUNNER_DEBUG": "true", "TOKEN": "s3cr3t"},
	}, &config.Config{})
	defer deregisterGlobalMasks(reporter)

	assert.True(t, reporter.debugOutputEnabled)
	assert.Equal(t, "debug is true", reporter.mask("debug is true"))
	assert.Equal(t, "***", reporter.mask("s3cr3t"))

	reporter.SetOutputs(map[string]string{"changed": "true"})
	assert.Equal(t, "true", reporter.outputs["changed"].value) // needs.<job>.outputs.changed == 'true' still works
}

// An output the server acknowledged is not reported again.
func TestReporter_OutputsSentOnce(t *testing.T) {
	client := mocks.NewClient(t)
	var reported []map[string]string
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			reported = append(reported, req.Msg.Outputs)
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{SentOutputs: slices.Collect(maps.Keys(req.Msg.Outputs))}), nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	r := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)

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

func TestReporter_ParseResult(t *testing.T) {
	r := &Reporter{}

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
			got, ok := r.parseResult(tt.input)
			assert.Equal(t, tt.wantOk, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A secret leaked in an encoded form — the shape it takes once an action puts it in a
// JSON body, a URL or a base64 payload — must be masked in the reported log as well.
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

// A server that acknowledges more rows than were sent must not take the runner
// process down with it.
func TestReporter_AckIndexBeyondBuffer(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
		return connect_go.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: 1000}), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))

	require.NoError(t, fire(reporter, logEntry{
		Message: "hello",
		Data:    map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true},
		Level:   logging.LevelInfo,
	}))

	require.NoError(t, reporter.ReportLog(false))
}

// Close has to survive giving up on a daemon still parked in an RPC.
func TestReporter_CloseWithStuckDaemon(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
			AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
		}), nil
	})
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
		return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
	}).Maybe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportInterval = 10 * time.Millisecond
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
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

// The zombie reaper marks a task failed, and cancelling the context is how the
// runner stops, so the two must not be conflated into "cancelled".
func TestReporter_ServerFailureResultIsReportedAsFailure(t *testing.T) {
	var lastState *runnerv1.TaskState
	client := mocks.NewClient(t)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
		lastState = req.Msg.State
		return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{
			State: &runnerv1.TaskState{Result: runnerv1.Result_RESULT_FAILURE},
		}), nil
	})
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
		return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
			AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
		}), nil
	}).Maybe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	close(reporter.daemon) // no daemon loop to acknowledge

	require.NoError(t, reporter.ReportState(false))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.NoError(t, reporter.Close(""))

	require.NotNil(t, lastState)
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, lastState.Result)
}

func TestReporter_NoteReport(t *testing.T) {
	hook := logtest.SetDefault(t)

	reporter := &Reporter{reportFailing: map[string]bool{}}

	// A pending output is not a transport failure and must not read as one.
	reporter.noteReport(metrics.LabelMethodUpdateTask, fmt.Errorf("wrapped: %w", errOutputsNotSent))
	assert.Empty(t, hook.AllRecords())

	// An outage logs once at each end, not every report interval for hours.
	reporter.noteReport(metrics.LabelMethodUpdateTask, errors.New("connection refused"))
	reporter.noteReport(metrics.LabelMethodUpdateTask, errors.New("connection refused"))
	require.Len(t, hook.AllRecords(), 1)
	assert.Contains(t, hook.LastRecord().Message, "connection refused")

	reporter.noteReport(metrics.LabelMethodUpdateTask, nil)
	require.Len(t, hook.AllRecords(), 2)
	assert.Contains(t, hook.LastRecord().Message, "reconnected")
}

// A job's final error can carry a secret, e.g. one interpolated into a failing
// expression, so Close must mask it like every other row.
func TestReporter_CloseMasksLastWords(t *testing.T) {
	const secret = "supersecret"
	var rows []*runnerv1.LogRow

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			rows = append(rows, req.Msg.Rows...)
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: req.Msg.Index + int64(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
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

// giteaLogModel mirrors how Gitea stores a task log: UpdateLog appends rows to one
// stream, and UpdateTask overwrites the per-step ranges the web UI slices that stream by
// (modules/actions/task_state.go, FullSteps).
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

// snapshot returns the accepted rows and how far the reported step ranges reach into
// them. The remainder is what the web UI renders under "Complete job".
func (m *giteaLogModel) snapshot() (rows, claimed int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows, m.claimed
}

// Regression test for https://gitea.com/gitea/runner/issues/1184.
func TestReporter_StepRangeCoversAckedRows(t *testing.T) {
	server := &giteaLogModel{}

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{
				AckIndex: server.appendRows(len(req.Msg.Rows)),
			}), nil
		},
	)
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, req *connect_go.Request[runnerv1.UpdateTaskRequest]) (*connect_go.Response[runnerv1.UpdateTaskResponse], error) {
			server.applyState(req.Msg.State)
			return connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	// Only the batch threshold may flush: the tickers are the periodic repair this test
	// must not depend on, and Close() is the one after the damage is done.
	cfg, _ := config.LoadDefault("")
	cfg.Runner.LogReportInterval = time.Hour
	cfg.Runner.LogReportMaxLatency = time.Hour
	cfg.Runner.StateReportInterval = time.Hour
	cfg.Runner.LogReportBatchSize = 5

	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)
	reporter.ResetSteps(newTestSteps(1))
	reporter.RunDaemon()
	defer func() {
		_ = reporter.Close("")
	}()

	stepData := map[string]any{"stage": "Main", "stepNumber": 0, "raw_output": true}

	// The step's first row is a transition, so it flushes state as well as logs.
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

// A flush the server took no rows for describes nothing new, so it must not spend an
// UpdateTask. UpdateTask is never registered on the client, so calling it fails the test.
func TestReporter_FlushWithoutAckedRowsSkipsState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taskCtx, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Once().Return(
		func(_ context.Context, _ *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: 0}), nil
		},
	)

	cfg, _ := config.LoadDefault("")
	reporter := NewReporter(ctx, cancel, client, &runnerv1.Task{Context: taskCtx}, cfg)

	// An idle job: no rows, so not even an UpdateLog.
	reporter.reportLogWithState()

	// A row the server declines to take: an UpdateLog, but still no step range to report.
	reporter.Logf("row")
	reporter.reportLogWithState()
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
		client.On("UpdateLog", mock.Anything, mock.Anything).Maybe().Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
			return connect_go.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: req.Msg.Index + int64(len(req.Msg.Rows))}), nil
		})
		client.On("UpdateTask", mock.Anything, mock.Anything).Maybe().Return(connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil)
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
			{cleanup, 0, "Complete job", unspecified, 11, 2}, // still logging after the job result
		}
		assert.Equal(t, want, reportedOf(t, reporter))

		send(t, reporter, logging.LevelInfo, "Removing images", map[string]any{"stage": runner.StageComplete})
		require.NoError(t, reporter.Close(""))
		want[5].result, want[5].logLen = success, 3
		assert.Equal(t, want, reportedOf(t, reporter))

		// the workflow steps keep what Gitea versions deriving them from the workflow file expect
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
			"Run echo 0":      failure, // not the result of its composite step
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

// logEntry is the record a job logger hands the Reporter, with the fields it accumulated as Data.
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
