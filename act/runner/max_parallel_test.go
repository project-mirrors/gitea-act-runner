// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"strings"
	"testing"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func TestMaxParallelStrategy(t *testing.T) {
	for _, tt := range []struct {
		name                string
		maxParallelString   string
		expectedMaxParallel int
	}{
		{"max-parallel-1", "1", 1},
		{"max-parallel-2", "2", 2},
		{"max-parallel-default", "", 4},
		{"max-parallel-10-clamped-to-combinations", "10", 5},
		{"max-parallel-invalid-falls-back-to-default", "tow", 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var rawMatrix yaml.Node
			require.NoError(t, rawMatrix.Encode(map[string][]any{"version": {1, 2, 3, 4, 5}}))
			job := &model.Job{Strategy: &model.Strategy{MaxParallelString: tt.maxParallelString, RawMatrix: rawMatrix}}

			matrixes, err := job.GetMatrixes()
			require.NoError(t, err)
			require.Len(t, matrixes, 5)
			assert.Equal(t, tt.expectedMaxParallel, maxParallelFor(job.Strategy, len(matrixes)))
		})
	}

	t.Run("deferred strategy", func(t *testing.T) {
		workflow, err := model.ReadWorkflow(strings.NewReader(`
jobs:
  test:
    if: false
    strategy: ${{ fromJSON('{"max-parallel":2,"fail-fast":false,"matrix":{"os":["ubuntu","windows"]}}') }}
`))
		require.NoError(t, err)
		job := workflow.Jobs["test"]
		rawStrategy := model.CloneYamlNode(job.RawStrategy)
		require.NoError(t, (&runnerImpl{config: &Config{Env: testCheckoutEnv()}}).NewPlanExecutor(&model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{{Workflow: workflow, JobID: "test"}}}}})(t.Context()))
		require.NotNil(t, job.Strategy)
		assert.False(t, job.Strategy.GetFailFast())
		assert.Equal(t, 2, maxParallelFor(job.Strategy, 5))
		matrixes, err := job.GetMatrixes()
		require.NoError(t, err)
		assert.Equal(t, []map[string]any{{"os": "ubuntu"}, {"os": "windows"}}, matrixes)
		assert.Equal(t, rawStrategy, job.RawStrategy)
	})
}

func TestNewPlanExecutorInvalidMatrix(t *testing.T) {
	var rawMatrix yaml.Node
	require.NoError(t, rawMatrix.Encode(map[string]any{
		"config": map[string]any{"nested": "value"},
	}))

	plan := &model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{{
		Workflow: &model.Workflow{Jobs: map[string]*model.Job{
			"test": {Strategy: &model.Strategy{RawMatrix: rawMatrix}},
		}},
		JobID: "test",
	}}}}}
	runner := &runnerImpl{config: &Config{Env: testCheckoutEnv()}}

	require.ErrorContains(t, runner.NewPlanExecutor(plan)(t.Context()), "could not get job matrix:")

	for _, strategy := range []string{
		`${{ fromJSON('invalid') }}`,
		`${{ '${{ inputs.unresolved }}' }}`,
	} {
		t.Run(strategy, func(t *testing.T) {
			workflow, err := model.ReadWorkflow(strings.NewReader("jobs:\n  test:\n    strategy: " + strategy))
			require.NoError(t, err)
			require.Error(t, runner.NewPlanExecutor(&model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{{Workflow: workflow, JobID: "test"}}}}})(t.Context()))
		})
	}
}
