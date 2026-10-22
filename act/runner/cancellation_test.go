// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"

	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.yaml.in/yaml/v4"
)

// TestCancelledJobStatusEnablesAlwaysAndCancelledSteps verifies that once a job is
// cancelled, getJobContext reports the "cancelled" status so the step `if` functions
// evaluate the way GitHub Actions does: cancelled()/always() are true, success()/failure()
// are false. A step that defaults to success() is therefore skipped while an always() step
// still runs. Before the fix the status could only ever be success/failure, so cancelled()
// was structurally impossible and cancel-only cleanup steps never ran.
func TestCancelledJobStatusEnablesAlwaysAndCancelledSteps(t *testing.T) {
	rc := createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
	})
	rc.markCancelled()

	// The core fix: the job status context now reports "cancelled" instead of being
	// pinned to success/failure.
	jobCtx := rc.getJobContext()
	require.Equal(t, "cancelled", jobCtx.Status)

	// Feed that status through the step-context expression functions, which is what a
	// step `if` evaluates. On a cancelled job only always()/cancelled() are true.
	interp := exprparser.NewInterpeter(
		&exprparser.EvaluationEnvironment{Job: jobCtx},
		exprparser.Config{Context: "step"},
	)
	for expr, want := range map[string]bool{
		"cancelled()":  true,
		"always()":     true,
		"success()":    false,
		"failure()":    false,
		"!cancelled()": false,
	} {
		got, err := interp.Evaluate(expr, exprparser.DefaultStatusCheckNone)
		require.NoErrorf(t, err, "Evaluate(%q)", expr)
		assert.Equalf(t, want, got, "Evaluate(%q) on a cancelled job", expr)
	}

	// A step without an `if` defaults to success() and must be skipped on cancel,
	// while an `if: always()` step must still run.
	disabled, err := interp.Evaluate("", exprparser.DefaultStatusCheckSuccess)
	require.NoError(t, err)
	assert.Equal(t, false, disabled, "default-success step must be skipped on a cancelled job")

	enabled, err := interp.Evaluate("always()", exprparser.DefaultStatusCheckSuccess)
	require.NoError(t, err)
	assert.Equal(t, true, enabled, "`if: always()` step must run on a cancelled job")
	setJobResult(t.Context(), rc, rc, true)
	assert.Equal(t, "cancelled", rc.Run.Job().Result)
}

func TestMainStepsExecutorRunsAlwaysStepsAfterInterruption(t *testing.T) {
	for name, wantError := range map[string]error{"cancel": context.Canceled, "timeout": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			rc := createIfTestRunContext(map[string]*model.Job{
				"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
			})
			var ctx context.Context
			var interrupt func()
			if wantError == context.Canceled {
				ctx, interrupt = context.WithCancel(t.Context())
				defer interrupt()
			} else {
				deadline := newControllableDeadlineContext(t.Context())
				ctx, interrupt = deadline, deadline.expire
			}

			var ran []string
			var laterStepCtxErr error
			err := newMainStepsExecutor(rc, []common.Executor{
				func(_ctx context.Context) error {
					ran = append(ran, "step1")
					interrupt()
					return nil
				},
				func(ctx context.Context) error {
					ran = append(ran, "always-step")
					laterStepCtxErr = ctx.Err()
					return nil
				},
			})(ctx)

			require.ErrorIs(t, err, wantError)
			assert.Equal(t, []string{"step1", "always-step"}, ran)
			require.NoError(t, laterStepCtxErr)
			assert.Equal(t, wantError == context.Canceled, rc.jobCancelled)
			if wantError == context.DeadlineExceeded {
				assert.True(t, rc.jobFailed)
				assert.Equal(t, "failure", rc.getJobContext().Status)
			}
		})
	}
}

func TestStepsExecutorRunsMainStepsAfterPreInterruption(t *testing.T) {
	for name, tt := range map[string]struct {
		cancel    bool
		preErr    error
		wantError error
	}{
		"cancel":  {cancel: true, wantError: context.Canceled},
		"failure": {preErr: assert.AnError, wantError: assert.AnError},
	} {
		t.Run(name, func(t *testing.T) {
			rc := createIfTestRunContext(map[string]*model.Job{
				"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
			})
			ctx := t.Context()
			var cancel context.CancelFunc
			if tt.cancel {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}

			var ran []string
			var mainStepCtxErr error
			err := newStepsExecutor(rc, []common.Executor{
				func(_ctx context.Context) error {
					ran = append(ran, "pre1")
					if tt.cancel {
						cancel()
					}
					return tt.preErr
				},
			}, []common.Executor{
				func(ctx context.Context) error {
					ran = append(ran, "always-step")
					mainStepCtxErr = ctx.Err()
					return nil
				},
			})(ctx)

			require.ErrorIs(t, err, tt.wantError)
			assert.Equal(t, []string{"pre1", "always-step"}, ran)
			require.NoError(t, mainStepCtxErr)
			assert.Equal(t, tt.cancel, rc.jobCancelled)
		})
	}
}

// TestPreStepFailureAffectsMainStepIfStatus verifies the status path used by real
// main-step `if` evaluation. A pre-step failure is not present in StepResults, so
// recording only the context job error is not enough: getJobContext must also report
// failure so success()-default main steps skip and failure() steps run.
func TestPreStepFailureAffectsMainStepIfStatus(t *testing.T) {
	rc := createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
	})
	ctx := common.WithJobErrorContainer(t.Context())

	reportStepError(ctx, rc, assert.AnError)

	assert.Equal(t, "failure", rc.getJobContext().Status)
	require.ErrorIs(t, common.JobError(ctx), assert.AnError)

	defaultStep := &stepRun{
		RunContext: rc,
		Step:       &model.Step{ID: "default-step"},
		env:        map[string]string{},
	}
	defaultEnabled, err := isStepEnabled(ctx, defaultStep.getIfExpression(ctx, stepStageMain), defaultStep, stepStageMain)
	require.NoError(t, err)
	assert.False(t, defaultEnabled, "default success() main step must skip after a pre-step failure")

	failureStep := &stepRun{
		RunContext: rc,
		Step: &model.Step{
			ID: "failure-step",
			If: yaml.Node{Value: "failure()"},
		},
		env: map[string]string{},
	}
	failureEnabled, err := isStepEnabled(ctx, failureStep.getIfExpression(ctx, stepStageMain), failureStep, stepStageMain)
	require.NoError(t, err)
	assert.True(t, failureEnabled, "failure() main step must run after a pre-step failure")
}

// TestPostStepsContextCancelledIsUsableForFailingStep guards against a panic: post/cleanup
// steps run on a context derived from the cancelled job context, and a failing post step
// records its error via common.SetJobError. If that derived context lacks a job-error container,
// SetJobError dereferences a nil map and panics. The post context must therefore be detached
// from cancellation (so the steps run) yet still carry a usable error container.
func TestPostStepsContextCancelledIsUsableForFailingStep(t *testing.T) {
	jobSpan := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}})
	cancelled, cancel := context.WithCancel(trace.ContextWithSpanContext(common.WithJobErrorContainer(t.Context()), jobSpan))
	cancel()
	require.ErrorIs(t, cancelled.Err(), context.Canceled)

	postCtx, done := postStepsContext(cancelled)
	defer done()

	// Detached from cancellation, so the post steps actually run.
	require.NoError(t, postCtx.Err(), "post context must not be cancelled")
	assert.Equal(t, jobSpan, trace.SpanContextFromContext(postCtx), "post steps must stay in the job's trace")

	// A failing post step records its error instead of panicking.
	require.NotPanics(t, func() {
		common.SetJobError(postCtx, assert.AnError)
	}, "a failing post step must not panic on the cancel path")
	assert.ErrorIs(t, common.JobError(postCtx), assert.AnError)
}

// TestPostStepsContextDeadlinePreservesJobError verifies the job-timeout path keeps the original
// job-error container (via context.WithoutCancel), so the timeout failure and any post-step error
// survive into the post phase and the job is still reported as failed.
func TestPostStepsContextDeadlinePreservesJobError(t *testing.T) {
	base := common.WithJobErrorContainer(t.Context())
	common.SetJobError(base, assert.AnError)
	expired, cancel := context.WithDeadline(base, time.Now().Add(-time.Hour))
	defer cancel()
	require.ErrorIs(t, expired.Err(), context.DeadlineExceeded)

	postCtx, done := postStepsContext(expired)
	defer done()

	require.NoError(t, postCtx.Err(), "post context must not carry the expired deadline")
	assert.ErrorIs(t, common.JobError(postCtx), assert.AnError, "the timeout job error must be preserved")
}

// reportStepError must treat a context.Canceled (e.g. a teardown-cancelled read) as an
// interruption, never a job failure.
func TestReportStepErrorTreatsCancelAsInterruption(t *testing.T) {
	rc := &RunContext{}

	// stray read cancellation while the job context is live: ignored, not a failure
	live := common.WithJobErrorContainer(t.Context())
	reportStepError(live, rc, context.Canceled)
	require.NoError(t, common.JobError(live))
	assert.False(t, rc.jobFailed)
	assert.False(t, rc.jobCancelled)

	// genuine job cancellation: recorded as cancelled, still not a failure
	cancelled, cancel := context.WithCancel(common.WithJobErrorContainer(t.Context()))
	cancel()
	reportStepError(cancelled, rc, context.Canceled)
	require.NoError(t, common.JobError(cancelled))
	assert.False(t, rc.jobFailed)
	assert.True(t, rc.jobCancelled)

	// a real error still fails the job
	failed := common.WithJobErrorContainer(t.Context())
	reportStepError(failed, rc, assert.AnError)
	require.ErrorIs(t, common.JobError(failed), assert.AnError)
	assert.True(t, rc.jobFailed)
}
