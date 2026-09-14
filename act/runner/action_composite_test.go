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
