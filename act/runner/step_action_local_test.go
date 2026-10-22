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
			StepResults: map[string]*model.StepResult{},
			ExprEval:    &expressionEvaluator{},
			Config: &Config{
				Workdir: "/tmp",
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
		},
		Step: &model.Step{
			ID:   "1",
			Uses: "./path/to/action",
		},
	}

	salm.On("readAction", sal.Step, filepath.Clean("/tmp/path/to/action"), "", mock.Anything, mock.Anything).
		Return(&model.Action{}, nil)

	expectStepFileCommands(t, cm)

	salm.On("runAction", sal, filepath.Clean("/tmp/path/to/action"), (*remoteAction)(nil)).Return(noopExecutor)

	err := sal.pre()(ctx)
	require.NoError(t, err)

	err = sal.main()(ctx)
	require.NoError(t, err)

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
						Config:       &Config{GitHubInstance: "https://github.com", ActionCacheDir: t.TempDir()},
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
