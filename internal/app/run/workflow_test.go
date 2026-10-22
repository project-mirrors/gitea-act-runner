// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"testing"

	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func Test_generateWorkflow(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		needs   map[string]*runnerv1.TaskNeed
		assert  func(t *testing.T, wf *model.Workflow)
		jobID   string
		wantErr bool
	}{
		{
			name: "has needs",
			payload: `
name: Build and deploy
on: push

jobs:
  job9:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - run: ./deploy --build ${{ needs.job1.outputs.output1 }}
      - run: ./deploy --build ${{ needs.job2.outputs.output2 }}
`,
			needs: map[string]*runnerv1.TaskNeed{
				"job1": {
					Outputs: map[string]string{
						"output1": "output1 value",
					},
					Result: runnerv1.Result_RESULT_SUCCESS,
				},
				"job2": {
					Outputs: map[string]string{
						"output2": "output2 value",
					},
					Result: runnerv1.Result_RESULT_SUCCESS,
				},
			},
			assert: func(t *testing.T, wf *model.Workflow) {
				require.Equal(t, []string{"job1", "job2"}, wf.GetJob("job9").Needs())
			},
			jobID: "job9",
		},
		{
			name: "no needs",
			payload: `
name: Simple workflow
on: push

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - run: echo "hello"
`,
			needs: map[string]*runnerv1.TaskNeed{},
			assert: func(t *testing.T, wf *model.Workflow) {
				job := wf.GetJob("test")
				require.Equal(t, []string{}, job.Needs())
				require.Len(t, job.Steps, 2)
			},
			jobID: "test",
		},
		{
			name: "needs list",
			payload: `
name: Workflow with list needs
on: push

jobs:
  deploy:
    needs: [build, test, lint]
    runs-on: ubuntu-latest
    steps:
      - run: echo "deploying"
`,
			needs: map[string]*runnerv1.TaskNeed{
				"build": {
					Outputs: map[string]string{},
					Result:  runnerv1.Result_RESULT_SUCCESS,
				},
				"test": {
					Outputs: map[string]string{
						"coverage": "80%",
					},
					Result: runnerv1.Result_RESULT_SUCCESS,
				},
				"lint": {
					Outputs: map[string]string{},
					Result:  runnerv1.Result_RESULT_FAILURE,
				},
			},
			assert: func(t *testing.T, wf *model.Workflow) {
				job := wf.GetJob("deploy")
				require.Equal(t, []string{"build", "lint", "test"}, job.Needs())
				require.Equal(t, "80%", wf.Jobs["test"].Outputs["coverage"])
				require.Equal(t, "failure", wf.Jobs["lint"].Result)
			},
			jobID: "deploy",
		},
		{
			name: "workflow env and defaults",
			payload: `
name: Complex workflow
on:
  push:
    branches: [main, develop]
  pull_request:
    types: [opened, synchronize]

env:
  NODE_ENV: production
  CI: "true"

jobs:
  build:
    runs-on: ubuntu-latest
    env:
      BUILD_TYPE: release
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-node@v3
        with:
          node-version: "18"
      - run: npm ci
      - run: npm run build
`,
			needs: map[string]*runnerv1.TaskNeed{},
			assert: func(t *testing.T, wf *model.Workflow) {
				require.Equal(t, "Complex workflow", wf.Name)
				require.Equal(t, "production", wf.Env["NODE_ENV"])
				require.Equal(t, "true", wf.Env["CI"])
				require.Len(t, wf.GetJob("build").Steps, 4)
			},
			jobID: "build",
		},
		{
			name: "job with container and services",
			payload: `
name: Integration tests
on: push

jobs:
  integration:
    runs-on: ubuntu-latest
    container:
      image: node:18
    services:
      postgres:
        image: postgres:15
    steps:
      - uses: actions/checkout@v3
      - run: npm test
`,
			needs: map[string]*runnerv1.TaskNeed{},
			assert: func(t *testing.T, wf *model.Workflow) {
				job := wf.GetJob("integration")
				require.Equal(t, "node:18", job.Container().Image)
				require.Equal(t, "postgres:15", job.Services["postgres"].Image)
			},
			jobID: "integration",
		},
		{
			name: "job with matrix strategy",
			payload: `
name: Matrix build
on: push

jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        go-version: ["1.21", "1.22"]
    steps:
      - uses: actions/checkout@v3
      - run: go test ./...
`,
			needs: map[string]*runnerv1.TaskNeed{},
			assert: func(t *testing.T, wf *model.Workflow) {
				matrixes, err := wf.GetJob("test").GetMatrixes()
				require.NoError(t, err)
				require.Len(t, matrixes, 2)
			},
			jobID: "test",
		},
		{
			name:    "special yaml characters in values",
			payload: "name: \"Special: characters & test\"\non: push\n\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: 'echo \"hello: world\"'\n      - run: 'echo \"quotes & ampersands\"'\n      - run: |\n          echo \"multiline\"\n          echo \"script\"\n",
			needs:   map[string]*runnerv1.TaskNeed{},
			assert: func(t *testing.T, wf *model.Workflow) {
				require.Equal(t, "Special: characters & test", wf.Name)
				require.Len(t, wf.GetJob("test").Steps, 3)
			},
			jobID: "test",
		},
		{
			name: "invalid yaml",
			payload: `
name: Bad workflow
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
    - run: echo "ok"
     bad-indent: true
`,
			needs:   map[string]*runnerv1.TaskNeed{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1, err := generateWorkflow(&runnerv1.Task{WorkflowPayload: []byte(tt.payload), Needs: tt.needs})
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			tt.assert(t, got)
			require.Equal(t, tt.jobID, got1)
		})
	}
}

func Test_yamlV4NodeRoundTrip(t *testing.T) {
	t.Run("marshal sequence node", func(t *testing.T) {
		node := yaml.Node{
			Kind: yaml.SequenceNode,
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "a"},
				{Kind: yaml.ScalarNode, Value: "b"},
				{Kind: yaml.ScalarNode, Value: "c"},
			},
		}

		out, err := yaml.Marshal(&node)
		require.NoError(t, err)
		require.Equal(t, "- a\n- b\n- c\n", string(out))
	})

	t.Run("unmarshal and re-marshal workflow", func(t *testing.T) {
		input := []byte("name: test\non: push\njobs:\n    build:\n        runs-on: ubuntu-latest\n        steps:\n            - run: echo hello\n")

		var wf map[string]any
		err := yaml.Unmarshal(input, &wf)
		require.NoError(t, err)
		require.Equal(t, "test", wf["name"])

		out, err := yaml.Marshal(wf)
		require.NoError(t, err)

		var wf2 map[string]any
		err = yaml.Unmarshal(out, &wf2)
		require.NoError(t, err)
		require.Equal(t, "test", wf2["name"])
	})

	t.Run("node kind constants", func(t *testing.T) {
		require.NotEqual(t, yaml.ScalarNode, yaml.SequenceNode)
		require.NotEqual(t, yaml.SequenceNode, yaml.MappingNode)
	})
}
