// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"testing"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompositeActionParity(t *testing.T) {
	t.Run("inherits contexts without leaking inputs", func(t *testing.T) {
		ctx := t.Context()
		strategy := &model.Strategy{MaxParallelString: "3"}
		parent := &RunContext{
			Config:        &Config{},
			Matrix:        map[string]any{"os": "linux"},
			Run:           &model.Run{JobID: "job", Workflow: &model.Workflow{Name: "workflow", Jobs: map[string]*model.Job{"job": {Strategy: strategy}}}},
			JobContainer:  &jobContainerMock{},
			platformImage: "-self-hosted",
		}
		composite, err := newCompositeRunContext(ctx, parent, &stepActionRemote{
			Step:       &model.Step{With: map[string]string{"SHARED": "outer"}},
			RunContext: parent,
			action:     &model.Action{Inputs: map[string]model.Input{"shared": {Default: "outer-default"}}},
			env:        map[string]string{"INPUT_SHARED": "outer"},
		}, "/action")
		require.NoError(t, err)

		assert.Same(t, strategy, composite.Run.Job().Strategy)
		assert.True(t, composite.IsHostEnv())
		interpolated, err := composite.NewExpressionEvaluator(ctx).Interpolate(ctx,
			"${{ matrix.os }}|${{ strategy.max-parallel }}|${{ inputs.shared }}")
		require.NoError(t, err)
		assert.Equal(t, "linux|3|outer", interpolated)
		assert.NotContains(t, composite.Env, "INPUT_SHARED")

		nestedEnv := composite.GetEnv()
		require.NoError(t, populateEnvsFromInput(ctx, &nestedEnv, &model.Action{Inputs: map[string]model.Input{"shared": {Default: "inner-default"}}}, composite))
		assert.Equal(t, "inner-default", nestedEnv["INPUT_SHARED"])
	})

	t.Run("propagates pre failures", func(t *testing.T) {
		setCloneExecutor(t, func(git.NewGitCloneExecutorInput) common.Executor { return common.NewErrorExecutor(assert.AnError) })
		rc := &RunContext{
			Config:       &Config{GitHubInstance: "github.com", ActionCacheDir: t.TempDir()},
			Run:          &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
			JobContainer: &jobContainerMock{},
		}

		require.ErrorIs(t, rc.compositeExecutor(&model.Action{Runs: model.ActionRuns{Using: "composite", Steps: []model.Step{{ID: "nested", Uses: "org/action@v1"}}}}).pre(t.Context()), assert.AnError)
	})
}

func TestAppendUniqueMasks(t *testing.T) {
	tests := []struct {
		name string
		dst  []string
		src  []string
		want []string
	}{
		{
			name: "appends new masks",
			dst:  []string{"a"},
			src:  []string{"b", "c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "skips masks already present",
			dst:  []string{"a", "b"},
			src:  []string{"a", "b"},
			want: []string{"a", "b"},
		},
		{
			name: "deduplicates within src",
			dst:  []string{"a"},
			src:  []string{"b", "b", "a"},
			want: []string{"a", "b"},
		},
		{
			name: "empty src leaves dst unchanged",
			dst:  []string{"a"},
			src:  nil,
			want: []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, appendUniqueMasks(tt.dst, tt.src))
		})
	}
}

// TestAppendUniqueMasksNoExponentialGrowth reproduces the exponential growth of
// the parent's Masks slice observed with nested/repeated composite actions. A
// composite RunContext is seeded with its parent's masks and the whole seeded
// slice was previously appended back into the parent, doubling its length on
// every composite action.
func TestAppendUniqueMasksNoExponentialGrowth(t *testing.T) {
	parentMasks := []string{"secret"}

	for range 20 {
		// compositeRC.Masks starts as a copy of the parent's masks (it is
		// seeded with parent.Masks in newCompositeRunContext).
		compositeMasks := make([]string, len(parentMasks))
		copy(compositeMasks, parentMasks)

		parentMasks = appendUniqueMasks(parentMasks, compositeMasks)
	}

	assert.Equal(t, []string{"secret"}, parentMasks)
}

func TestCompositeStepIfReadsJobStatusExceptInItsOwnMainSteps(t *testing.T) {
	failStep := func(rc *RunContext) {
		rc.StepResults["failed"] = &model.StepResult{Conclusion: model.StepStatusFailure}
	}
	tests := []struct {
		name   string
		setup  func(job, composite *RunContext)
		nested bool
		stage  stepStage
		want   map[string]bool
	}{
		{"post-if reads the failed job", func(job, _ *RunContext) { failStep(job) }, false, stepStagePost, map[string]bool{"success()": false, "failure()": true, "job.status == 'failure'": true}},
		{"main if ignores the failed job", func(job, _ *RunContext) { failStep(job) }, false, stepStageMain, map[string]bool{"success()": true, "failure()": false}},
		{"main if reads the composite's own failure", func(_, composite *RunContext) { failStep(composite) }, false, stepStageMain, map[string]bool{"success()": false, "failure()": true}},
		{"nested main if ignores the outer composite's failure", func(_, composite *RunContext) { failStep(composite) }, true, stepStageMain, map[string]bool{"success()": true}},
		{"main if sees the cancelled job", func(job, _ *RunContext) { job.markCancelled() }, false, stepStageMain, map[string]bool{"cancelled()": true, "success()": false}},
		{"post-if sees the cancelled job", func(job, _ *RunContext) { job.markCancelled() }, false, stepStagePost, map[string]bool{"cancelled()": true, "success()": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := makeTestRC(t, "runs-on: ubuntu-latest")
			composite := newTestCompositeRunContext(t, job)
			rc := composite
			if tt.nested {
				rc = newTestCompositeRunContext(t, composite)
			}
			tt.setup(job, composite)
			step := &stepRun{RunContext: rc}
			for expr, want := range tt.want {
				enabled, err := isStepEnabled(t.Context(), expr, step, tt.stage)
				require.NoError(t, err)
				assert.Equal(t, want, enabled, expr)
			}
		})
	}
}

func newTestCompositeRunContext(t *testing.T, parent *RunContext) *RunContext {
	composite, err := newCompositeRunContext(t.Context(), parent, &stepActionRemote{
		Step:       &model.Step{},
		RunContext: parent,
		action:     &model.Action{},
		env:        map[string]string{},
	}, "/action")
	require.NoError(t, err)
	return composite
}
