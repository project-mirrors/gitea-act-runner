// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

type stepActionMocks struct {
	mock.Mock
}

func (salm *stepActionMocks) runAction(step actionStep, actionDir string, remoteAction *remoteAction) common.Executor {
	args := salm.Called(step, actionDir, remoteAction)
	return args.Get(0).(func(context.Context) error)
}

func (salm *stepActionMocks) readAction(_ctx context.Context, step *model.Step, actionDir, actionPath string, readFile actionYamlReader, writeFile fileWriter) (*model.Action, error) {
	args := salm.Called(step, actionDir, actionPath, readFile, writeFile)
	return args.Get(0).(*model.Action), args.Error(1)
}

func expectStepFileCommands(t *testing.T, cm *containerMock) {
	t.Helper()
	cm.On("Copy", "/var/run/act", mock.AnythingOfType("[]*container.FileEntry")).Return(noopExecutor)
	for _, filename := range []string{"envs.txt", "statecmd.txt", "outputcmd.txt"} {
		cm.On("UpdateFromEnv", "/var/run/act/workflow/"+filename, mock.AnythingOfType("*map[string]string")).Return(noopExecutor)
	}
	cm.On("GetContainerArchive", t.Context(), "/var/run/act/workflow/pathcmd.txt").Return(io.NopCloser(&bytes.Buffer{}), nil)
}

func TestStepActionLocalTest(t *testing.T) {
	ctx := t.Context()

	cm := &containerMock{}
	salm := &stepActionMocks{}

	sal := &stepActionLocal{
		readAction: salm.readAction,
		runAction:  salm.runAction,
		RunContext: &RunContext{
			StepResults: map[string]*model.StepResult{"previous": {Outputs: map[string]string{"value": "previous"}}},
			ExprEval:    &expressionEvaluator{},
			Config: &Config{
				Workdir: "/tmp",
				Env:     testCheckoutEnv(),
			},
			Run: &model.Run{
				JobID: "1",
				Workflow: &model.Workflow{
					Jobs: map[string]*model.Job{
						"1": {},
					},
				},
			},
			JobContainer: cm,
			Env:          map[string]string{"GITHUB_ACTION_REPOSITORY": "other/action", "GITHUB_ACTION_REF": "v1"},
			Matrix:       map[string]any{"value": "matrix"},
			actionInputs: map[string]any{"shared": "input"},
		},
		Step: &model.Step{
			ID:   "1",
			Uses: "./path/to/action",
			If: yaml.Node{Value: "github.action_repository == 'other/action' && github.action_ref == 'v1' && " +
				"env.STEP_VALUE == 'step' && env.GITHUB_ACTION_REPOSITORY == 'forged/repo' && env.GITHUB_ACTION_REF == 'forged-ref' && " +
				"steps.previous.outputs.value == 'previous' && inputs.shared == 'input' && matrix.value == 'matrix'"},
		},
	}
	require.NoError(t, sal.Step.Env.Encode(map[string]string{"STEP_VALUE": "step", "GITHUB_ACTION_REPOSITORY": "forged/repo", "GITHUB_ACTION_REF": "forged-ref"}))
	require.NoError(t, sal.Step.RawWith.Encode(map[string]string{"identity": "${{ github.action_repository }}|${{ github.action_ref }}"}))

	salm.On("readAction", sal.Step, filepath.Clean("/tmp/path/to/action"), "", mock.Anything, mock.Anything).
		Return(&model.Action{}, nil)

	expectStepFileCommands(t, cm)

	salm.On("runAction", sal, filepath.Clean("/tmp/path/to/action"), (*remoteAction)(nil)).Return(func(ctx context.Context) error {
		sal.RunContext.setOutput(ctx, map[string]string{"name": "value"}, "ran")
		return nil
	})

	err := sal.pre()(ctx)
	require.NoError(t, err)

	err = sal.main()(ctx)
	require.NoError(t, err)
	assert.Empty(t, sal.env["GITHUB_ACTION_REPOSITORY"])
	assert.Empty(t, sal.env["GITHUB_ACTION_REF"])
	assert.Equal(t, "|", sal.Step.With["identity"])
	assert.Equal(t, "ran", sal.RunContext.StepResults["1"].Outputs["value"])
	assert.Equal(t, "other/action", sal.RunContext.Env["GITHUB_ACTION_REPOSITORY"])
	assert.Equal(t, "v1", sal.RunContext.Env["GITHUB_ACTION_REF"])

	cm.AssertExpectations(t)
	salm.AssertExpectations(t)
}

func TestStepActionPost(t *testing.T) {
	for _, kind := range []string{"local", "remote"} {
		t.Run(kind, func(t *testing.T) {
			for _, test := range []struct {
				name   string
				status model.StepStatus
				postIf string
				run    bool
			}{
				{"main-success", model.StepStatusSuccess, "always()", true},
				{"main-failed", model.StepStatusFailure, "always()", true},
				{"skip-if-failed", model.StepStatusFailure, "success()", false},
				{"skip-if-main-skipped", model.StepStatusSkipped, "always()", false},
			} {
				t.Run(test.name, func(t *testing.T) {
					ctx := t.Context()
					cm := &containerMock{}
					rc := &RunContext{
						Config:       &Config{GitHubInstance: "https://github.com", ActionCacheDir: t.TempDir(), PresetGitHubContext: &model.GithubContext{}},
						JobContainer: cm,
						Run:          &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
						StepResults:  map[string]*model.StepResult{"step": {Conclusion: test.status}},
					}
					rc.ExprEval = rc.NewExpressionEvaluator(ctx)
					action := &model.Action{Runs: model.ActionRuns{Using: "node16", Post: "post.js", PostIf: test.postIf}}
					var currentStep step
					if kind == "remote" {
						currentStep = &stepActionRemote{
							RunContext:   rc,
							Step:         &model.Step{ID: "step", Uses: "remote/action@v1"},
							action:       action,
							env:          map[string]string{},
							remoteAction: mustNewRemoteAction(t, "remote/action@v1"),
						}
						if test.status == model.StepStatusSuccess {
							rc.IntraActionState = map[string]map[string]string{"step": {"key": "value"}}
						}
					} else {
						currentStep = &stepActionLocal{
							RunContext: rc,
							Step:       &model.Step{ID: "step", Uses: "./local/action"},
							action:     action,
							env:        map[string]string{},
						}
					}

					if test.run {
						cm.On("Exec", mock.MatchedBy(func(args []string) bool {
							if len(args) != 3 || args[0] != "node" || args[1] != "--preserve-symlinks-main" {
								return false
							}
							if kind == "local" {
								return strings.HasSuffix(args[2], "runner/local/action/post.js")
							}
							return strings.Contains(args[2], "post.js")
						}), *currentStep.getEnv(), "", "").Return(noopExecutor)
						expectStepFileCommands(t, cm)
					}

					require.NoError(t, currentStep.post()(ctx))
					if rc.IntraActionState != nil {
						assert.Equal(t, "value", (*currentStep.getEnv())["STATE_key"])
					}
					assert.NotContains(t, rc.StepResults, "post-step")
					cm.AssertExpectations(t)
				})
			}
		})
	}
}
