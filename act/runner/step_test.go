// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/telemetry"

	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	yaml "go.yaml.in/yaml/v4"
)

func TestMergeIntoMap(t *testing.T) {
	table := []struct {
		name     string
		target   map[string]string
		maps     []map[string]string
		expected map[string]string
	}{
		{
			name:     "testEmptyMap",
			target:   map[string]string{},
			maps:     []map[string]string{},
			expected: map[string]string{},
		},
		{
			name:   "testMergeIntoEmptyMap",
			target: map[string]string{},
			maps: []map[string]string{
				{
					"key1": "value1",
					"key2": "value2",
				}, {
					"key2": "overridden",
					"key3": "value3",
				},
			},
			expected: map[string]string{
				"key1": "value1",
				"key2": "overridden",
				"key3": "value3",
			},
		},
		{
			name: "testMergeIntoExistingMap",
			target: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
			maps: []map[string]string{
				{
					"key1": "overridden",
				},
			},
			expected: map[string]string{
				"key1": "overridden",
				"key2": "value2",
			},
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			mergeIntoMapCaseSensitive(tt.target, tt.maps...)
			assert.Equal(t, tt.expected, tt.target)
			mergeIntoMapCaseInsensitive(tt.target, tt.maps...)
			assert.Equal(t, tt.expected, tt.target)
		})
	}
}

type stepMock struct {
	mock.Mock
	step
}

func (sm *stepMock) pre() common.Executor {
	args := sm.Called()
	return args.Get(0).(func(context.Context) error)
}

func (sm *stepMock) main() common.Executor {
	args := sm.Called()
	return args.Get(0).(func(context.Context) error)
}

func (sm *stepMock) post() common.Executor {
	args := sm.Called()
	return args.Get(0).(func(context.Context) error)
}

func (sm *stepMock) getRunContext() *RunContext {
	args := sm.Called()
	return args.Get(0).(*RunContext)
}

func (sm *stepMock) getStepModel() *model.Step {
	args := sm.Called()
	return args.Get(0).(*model.Step)
}

func (sm *stepMock) getEnv() *map[string]string {
	args := sm.Called()
	return args.Get(0).(*map[string]string)
}

func TestSetupEnv(t *testing.T) {
	rc := &RunContext{
		Config: &Config{
			Env: mergeMaps(testCheckoutEnv(), map[string]string{"GITHUB_RUN_ID": "runId"}),
		},
		Run: &model.Run{
			JobID: "1",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"1": {
						Env: yaml.Node{
							Value: "JOB_KEY: jobvalue",
						},
					},
				},
			},
		},
		Env: map[string]string{
			"RC_KEY": "rcvalue",
		},
		JobContainer: &containerMock{},
	}
	step := &model.Step{}
	require.NoError(t, step.RawWith.Encode(map[string]string{"STEP_WITH": "with-value", "ID": "${{ fromJSON('1234567') }}"}))
	env := map[string]string{}

	testStep := &stepRun{RunContext: rc, Step: step, env: env}
	require.NoError(t, setupEnv(t.Context(), testStep))
	require.NoError(t, setupInputs(t.Context(), testStep))

	for _, suffix := range []string{"REF", "REF_NAME", "REF_TYPE", "SHA", "WORKSPACE", "REPOSITORY", "REPOSITORY_OWNER", "ACTOR"} {
		delete(env, "GITHUB_"+suffix)
		delete(env, "GITEA_"+suffix)
	}
	delete(env, "RUNNER_NAME")
	delete(env, "RUNNER_WORKSPACE")

	expected := map[string]string{
		"ACT":                      "true",
		"ACT_SKIP_CHECKOUT":        "true",
		"CI":                       "true",
		"GITHUB_ACTION":            "",
		"GITHUB_ACTIONS":           "true",
		"GITHUB_ACTION_PATH":       "",
		"GITHUB_ACTION_REF":        "",
		"GITHUB_ACTION_REPOSITORY": "",
		"GITHUB_BASE_REF":          "",
		"GITHUB_EVENT_NAME":        "",
		"GITHUB_EVENT_PATH":        "/var/run/act/workflow/event.json",
		"GITHUB_GRAPHQL_URL":       "",
		"GITHUB_HEAD_REF":          "",
		"GITHUB_JOB":               "1",
		"GITHUB_REF_PROTECTED":     "false",
		"GITHUB_RETENTION_DAYS":    "0",
		"GITHUB_RUN_ATTEMPT":       "",
		"GITHUB_RUN_ID":            "runId",
		"GITHUB_RUN_NUMBER":        "1",
		"GITHUB_WORKFLOW":          "",
		"INPUT_ID":                 "1234567",
		"INPUT_STEP_WITH":          "with-value",
		"RC_KEY":                   "rcvalue",
		"RUNNER_ENVIRONMENT":       "self-hosted",
		"RUNNER_PERFLOG":           "/dev/null",
		"RUNNER_TRACKING_ID":       "",
	}
	for name, value := range expected {
		if suffix, ok := strings.CutPrefix(name, "GITHUB_"); ok {
			expected["GITEA_"+suffix] = value
		}
	}
	assert.Equal(t, expected, env)

	for _, expression := range []string{"inputs.args", "env.ARGS"} {
		t.Run("deferred inputs from "+expression, func(t *testing.T) {
			workflow, err := model.ReadWorkflow(strings.NewReader(`
env: ${{ fromJSON(matrix.env) }}
jobs:
  test:
    env:
      PRIORITY: ${{ matrix.priority }}
    defaults:
      run: ${{ fromJSON(matrix.defaults) }}
    container:
      image: node:20
      env:
        CONTAINER: ${{ env.PRIORITY }}
    steps:
      - uses: ./act
        env:
          ARGS: ${{ inputs.args }}
        with: ${{ fromJSON(` + expression + `) }}
`))
			require.NoError(t, err)
			job := workflow.GetJob("test")
			rawEnv := model.CloneYamlNode(workflow.RawEnv)
			rawWith := model.CloneYamlNode(job.Steps[0].RawWith)
			for _, value := range []string{"first", "second"} {
				t.Run(value, func(t *testing.T) {
					t.Parallel()
					rc, err := (&runnerImpl{config: &Config{PresetGitHubContext: &model.GithubContext{}}}).newRunContext(t.Context(), &model.Run{Workflow: workflow, JobID: "test"}, map[string]any{"env": `{"WORKFLOW":"` + value + `${{ github.job }}","PRIORITY":"workflow"}`, "priority": "${{ github.job }}", "defaults": `{"shell":"` + value + `"}`})
					require.NoError(t, err)
					rc.workflowCallInputs = map[string]any{"args": `{"name":"` + value + `${{ github.job }}","fetch-depth":1234567}`}
					enabled, err := rc.isEnabled(t.Context())
					require.NoError(t, err)
					require.True(t, enabled)
					require.NoError(t, evaluateJobEnvAndDefaults(t.Context(), rc))
					assert.Equal(t, model.RunDefaults{Shell: value}, rc.jobRunDefaults)
					step := &stepRun{RunContext: rc, Step: job.Steps[0].Clone(), env: map[string]string{}}
					require.NoError(t, setupEnv(t.Context(), step))
					require.NoError(t, setupInputs(t.Context(), step))
					assert.Equal(t, map[string]string{"ARGS": `${{ inputs.args }}`, "INPUT_NAME": value + "${{ github.job }}", "INPUT_FETCH-DEPTH": "1234567"}, step.Step.GetEnv())
					assert.Equal(t, value+"${{ github.job }}", step.env["INPUT_NAME"])
					assert.Equal(t, value+"${{ github.job }}", step.env["WORKFLOW"])
					assert.Equal(t, "${{ github.job }}", step.env["PRIORITY"])
					assert.Equal(t, "${{ github.job }}", step.env["CONTAINER"])
					assert.Equal(t, rawEnv, workflow.RawEnv)
					assert.Equal(t, rawWith, job.Steps[0].RawWith)
					assert.Nil(t, workflow.Env)
					assert.Nil(t, job.Steps[0].With)
				})
			}
		})
	}

	t.Run("rejects inputs that still contain an expression", func(t *testing.T) {
		workflow, err := model.ReadWorkflow(strings.NewReader("jobs:\n  test:\n    steps:\n      - uses: ./act\n        with: ${{ inputs.args }}\n"))
		require.NoError(t, err)
		rc, err := (&runnerImpl{config: &Config{PresetGitHubContext: &model.GithubContext{}}}).newRunContext(t.Context(), &model.Run{Workflow: workflow, JobID: "test"}, nil)
		require.NoError(t, err)
		rc.workflowCallInputs = map[string]any{"args": "${{ inputs.unresolved }}"}
		require.ErrorContains(t, setupInputs(t.Context(), &stepRun{RunContext: rc, Step: workflow.Jobs["test"].Steps[0].Clone(), env: map[string]string{}}), "with:")
	})

	t.Run("whole step environment expression is evaluated once", func(t *testing.T) {
		rc := createRunContext(t)
		rc.Matrix["environment"] = `{"NAME":"dynamic${{ github.job }}","COUNT":12}`
		step := &stepRun{RunContext: rc, Step: &model.Step{}, env: map[string]string{}}
		require.NoError(t, step.Step.Env.Encode("${{ fromJSON(matrix.environment) }}"))

		require.NoError(t, setupEnv(t.Context(), step))
		assert.Equal(t, "dynamic${{ github.job }}", step.env["NAME"])
		assert.Equal(t, "12", step.env["COUNT"])
	})
}

func TestIsStepEnabled(t *testing.T) {
	for _, test := range []struct {
		name    string
		result  *model.StepResult
		success bool
	}{
		{"no prior steps", nil, true},
		{"prior step succeeded", &model.StepResult{Conclusion: model.StepStatusSuccess}, true},
		{"prior step failed", &model.StepResult{Conclusion: model.StepStatusFailure}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, condition := range []struct {
				expression string
				want       bool
			}{{"success()", test.success}, {"failure()", !test.success}, {"always()", true}} {
				t.Run(condition.expression, func(t *testing.T) {
					step := &stepRun{
						RunContext: &RunContext{
							Config:      &Config{PresetGitHubContext: &model.GithubContext{}},
							Run:         &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
							StepResults: map[string]*model.StepResult{},
						},
						Step: &model.Step{If: yaml.Node{Value: condition.expression}},
					}
					if test.result != nil {
						step.RunContext.StepResults["prior"] = test.result
					}
					enabled, err := isStepEnabled(t.Context(), step.getIfExpression(t.Context(), stepStageMain), step, stepStageMain)
					require.NoError(t, err)
					assert.Equal(t, condition.want, enabled)
				})
			}
		})
	}

	t.Run("env and step inputs are unavailable at every stage", func(t *testing.T) {
		step := &stepRun{
			RunContext: &RunContext{
				Config: &Config{PresetGitHubContext: &model.GithubContext{}},
				Run:    &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
				Env:    map[string]string{"INPUT_FORGED": "leaked"},
			},
			Step: &model.Step{},
			env:  map[string]string{"INPUT_FORGED": "leaked"},
		}
		for _, stage := range []stepStage{stepStageMain, stepStagePost} {
			enabled, err := isStepEnabled(t.Context(), "inputs.forged", step, stage)
			require.NoError(t, err)
			assert.False(t, enabled)
		}
	})
}

func TestIsContinueOnError(t *testing.T) {
	for _, test := range []struct {
		name, expression string
		env              map[string]string
		want, wantError  bool
	}{
		{name: "absent"},
		{name: "explicit true", expression: "true", want: true},
		{name: "explicit false", expression: "false"},
		{name: "expression true", expression: "${{ 'test' == 'test' }}", want: true},
		{name: "expression false", expression: "${{ 'test' != 'test' }}"},
		{name: "step inputs are unavailable", expression: "${{ inputs.forged }}", env: map[string]string{"INPUT_FORGED": "true"}},
		{name: "invalid expression", expression: "${{ 'test' != test }}", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			step := &stepRun{
				RunContext: &RunContext{
					Config: &Config{PresetGitHubContext: &model.GithubContext{}},
					Run:    &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
				},
				Step: &model.Step{},
				env:  test.env,
			}
			continueOnError, err := isContinueOnError(t.Context(), test.expression, step)
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, test.want, continueOnError)
		})
	}
}

func TestRunStepExecutorDoesNotLeakRefusalToNextStep(t *testing.T) {
	cm := &containerMock{}
	cm.On("Copy", mock.Anything, mock.Anything).Return(noopExecutor)
	cm.On("UpdateFromEnv", mock.Anything, mock.Anything).Return(noopExecutor)

	rc := &RunContext{
		Config: &Config{Env: testCheckoutEnv()},
		Run: &model.Run{
			JobID:    "1",
			Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}},
		},
		Env:          map[string]string{},
		StepResults:  map[string]*model.StepResult{},
		JobContainer: cm,
	}
	rc.ExprEval = rc.NewExpressionEvaluator(t.Context())
	ctx := common.WithDryrun(t.Context(), true)

	rc.commandHandler(ctx)("::set-env name=setup::y\n")
	stepSetup := &stepRun{RunContext: rc, Step: &model.Step{ID: "setup"}, env: map[string]string{}}
	require.NoError(t, runStepExecutor(stepSetup, stepStageMain, noopExecutor)(ctx))

	stepA := &stepRun{RunContext: rc, Step: &model.Step{ID: "a"}, env: map[string]string{}}
	errA := runStepExecutor(stepA, stepStageMain, func(context.Context) error {
		rc.commandHandler(ctx)("::set-env name=x::y\n")
		return errors.New("boom")
	})(ctx)
	require.ErrorContains(t, errA, "boom")

	stepB := &stepRun{RunContext: rc, Step: &model.Step{ID: "b", If: yaml.Node{Value: "always()"}}, env: map[string]string{}}
	require.NoError(t, runStepExecutor(stepB, stepStageMain, noopExecutor)(ctx))
}

type fileCommandTestContainer struct {
	*container.HostEnvironment
}

func (c *fileCommandTestContainer) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return c.HostEnvironment.UpdateFromEnv(srcPath, env)(ctx)
	}
}

func (c *fileCommandTestContainer) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.HostEnvironment.GetContainerArchive(ctx, srcPath)
}

func TestRunStepExecutorParity(t *testing.T) {
	newStep := func(t *testing.T, stepModel *model.Step) *stepRun {
		rc := createRunContext(t)
		rc.JobContainer = &container.HostEnvironment{ActPath: t.TempDir()}
		rc.ExprEval = rc.NewExpressionEvaluator(t.Context())
		return &stepRun{RunContext: rc, Step: stepModel, env: map[string]string{}}
	}
	badExpression := "${{ 'test' != test }}"
	for _, test := range []struct {
		name, wantError string
		step            *model.Step
		executor        common.Executor
	}{
		{"condition error", "if-expression", &model.Step{ID: "condition", If: yaml.Node{Value: badExpression}}, noopExecutor},
		{"continue-on-error expression error", "continue-on-error expression", &model.Step{ID: "continue", RawContinueOnError: badExpression}, common.NewErrorExecutor(assert.AnError)},
	} {
		t.Run(test.name, func(t *testing.T) {
			step := newStep(t, test.step)
			logger, hook := logtest.NewLogger()
			err := runStepExecutor(step, stepStageMain, test.executor)(common.WithLogger(t.Context(), logger))

			require.ErrorContains(t, err, test.wantError)
			assert.Equal(t, model.StepStatusFailure, step.RunContext.StepResults[test.step.ID].Conclusion)
			assert.Equal(t, model.StepStatusFailure, lastRecordAttr(hook, "stepResult"))
		})
	}

	t.Run("only a step that runs is marked started", func(t *testing.T) {
		for _, test := range []struct {
			condition string
			started   int
		}{{"true", 1}, {"false", 0}} {
			logger, hook := logtest.NewLogger()
			step := newStep(t, &model.Step{ID: "pre", If: yaml.Node{Value: test.condition}})
			require.NoError(t, runStepExecutor(step, stepStagePre, noopExecutor)(common.WithLogger(context.Background(), logger)))
			started := 0
			for _, entry := range hook.AllRecords() {
				if v, _ := entry.Attr(StepStartedField); v == true {
					started++
				}
			}
			assert.Equal(t, test.started, started, test.condition)
		}
	})

	t.Run("file command error honors continue-on-error and keeps the step summary", func(t *testing.T) {
		step := newStep(t, &model.Step{ID: "commands", RawContinueOnError: "true"})
		logger, hook := logtest.NewLogger()
		err := runStepExecutor(step, stepStageMain, func(context.Context) error {
			require.NoError(t, os.WriteFile(step.env["GITHUB_STEP_SUMMARY"], []byte("report"), 0o600))
			require.NoError(t, os.WriteFile(step.env["GITHUB_ENV"], []byte("GOOD=1\nmalformed\n"), 0o600))
			require.NoError(t, os.WriteFile(step.env["GITHUB_OUTPUT"], []byte("kept=value\n"), 0o600))
			require.NoError(t, os.WriteFile(step.env["GITHUB_STATE"], []byte("saved=value\n"), 0o600))
			return nil
		})(common.WithLogger(t.Context(), logger))

		require.NoError(t, err)
		result := step.RunContext.StepResults[step.Step.ID]
		assert.Equal(t, model.StepStatusFailure, result.Outcome)
		assert.Equal(t, model.StepStatusSuccess, result.Conclusion)
		assert.Equal(t, model.StepStatusSuccess, lastRecordAttr(hook, "stepResult"))
		assert.Equal(t, "1", step.RunContext.Env["GOOD"])
		assert.Equal(t, "value", result.Outputs["kept"])
		assert.Equal(t, "value", step.RunContext.IntraActionState[step.Step.ID]["saved"])
		for _, name := range []string{"GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_STATE", "GITHUB_PATH"} {
			contents, readErr := os.ReadFile(step.env[name])
			require.NoError(t, readErr)
			assert.Empty(t, contents)
		}
		summary, err := os.ReadFile(step.env["GITHUB_STEP_SUMMARY"])
		require.NoError(t, err)
		assert.Equal(t, "report", string(summary))
	})

	t.Run("a broken summary path still resets the command files", func(t *testing.T) {
		step := newStep(t, &model.Step{ID: "noop"})
		require.NoError(t, os.MkdirAll(filepath.Join(step.RunContext.JobContainer.GetActPath(), "workflow", "step-summary-0.md"), 0o700))
		require.NoError(t, runStepExecutor(step, stepStageMain, noopExecutor)(t.Context()))
	})

	t.Run("command file initialization failure prevents execution and stale commands", func(t *testing.T) {
		step := newStep(t, &model.Step{ID: "commands"})
		workflowDir := filepath.Join(step.RunContext.JobContainer.GetActPath(), "workflow")
		require.NoError(t, os.MkdirAll(filepath.Join(workflowDir, "outputcmd.txt"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(workflowDir, "envs.txt"), []byte("STALE=1\n"), 0o600))
		executed := false
		err := runStepExecutor(step, stepStageMain, func(context.Context) error {
			executed = true
			return nil
		})(t.Context())

		require.Error(t, err)
		assert.False(t, executed)
		assert.Empty(t, step.RunContext.Env["STALE"])
		assert.Equal(t, model.StepStatusFailure, step.RunContext.StepResults[step.Step.ID].Conclusion)
	})

	t.Run("file commands survive a job timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			step := newStep(t, &model.Step{ID: "interrupted"})
			ctx, cancel := applyJobTimeout(t.Context(), step.RunContext, &model.Job{TimeoutMinutes: "1"})
			defer cancel()
			step.RunContext.JobContainer = &fileCommandTestContainer{step.RunContext.JobContainer.(*container.HostEnvironment)}
			err := runStepExecutor(step, stepStageMain, func(execCtx context.Context) error {
				for name, content := range map[string]string{
					"GITHUB_ENV": "KEPT_ENV=value\n", "GITHUB_STATE": "saved=value\n",
					"GITHUB_OUTPUT": "kept=value\n", "GITHUB_PATH": "/kept/bin\n",
				} {
					require.NoError(t, os.WriteFile(step.env[name], []byte(content), 0o600))
				}
				<-execCtx.Done()
				return execCtx.Err()
			})(ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Equal(t, "value", step.RunContext.Env["KEPT_ENV"])
			assert.Equal(t, "value", step.RunContext.IntraActionState[step.Step.ID]["saved"])
			assert.Equal(t, "value", step.RunContext.StepResults[step.Step.ID].Outputs["kept"])
			assert.Contains(t, step.RunContext.ExtraPath, "/kept/bin")
		})
	})

	t.Run("composite child files do not leak", func(t *testing.T) {
		outer := newStep(t, &model.Step{ID: "outer"})
		childRC := &RunContext{
			Config: outer.RunContext.Config, Run: outer.RunContext.Run, Env: map[string]string{}, StepResults: map[string]*model.StepResult{},
			JobContainer: outer.RunContext.JobContainer, Parent: outer.RunContext,
		}
		childRC.ExprEval = childRC.NewExpressionEvaluator(t.Context())
		child := &stepRun{RunContext: childRC, Step: &model.Step{ID: "child"}, env: map[string]string{}}

		err := runStepExecutor(outer, stepStageMain, func(ctx context.Context) error {
			require.NoError(t, runStepExecutor(child, stepStageMain, func(context.Context) error {
				require.NoError(t, os.WriteFile(child.env["GITHUB_OUTPUT"], []byte("declared=child\nundeclared=leak\n"), 0o600))
				require.NoError(t, os.WriteFile(child.env["GITHUB_STATE"], []byte("saved=child\n"), 0o600))
				return nil
			})(ctx))
			outer.RunContext.setOutput(ctx, map[string]string{"name": "declared"}, "outer")
			return nil
		})(t.Context())

		require.NoError(t, err)
		assert.Equal(t, map[string]string{"declared": "outer"}, outer.RunContext.StepResults[outer.Step.ID].Outputs)
		assert.Equal(t, map[string]string{"declared": "child", "undeclared": "leak"}, childRC.StepResults[child.Step.ID].Outputs)
		assert.Empty(t, outer.RunContext.IntraActionState)
		assert.Equal(t, "child", childRC.IntraActionState[child.Step.ID]["saved"])
	})

	t.Run("stages and composite steps are traced under Gitea's step names", func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		telemetry.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
		t.Cleanup(func() { telemetry.SetTracerProvider(tracenoop.NewTracerProvider()) })
		ctx, _ := telemetry.StartJob(t.Context(), &runnerv1.Task{Id: 7})
		outer := newStep(t, &model.Step{ID: "outer", Uses: "actions/cache@v4", Number: 3})
		child := newStep(t, &model.Step{ID: "child", Name: "Restore", Number: 1})
		skipped := newStep(t, &model.Step{ID: "skipped", Name: "Deploy", Number: 4, If: yaml.Node{Value: "false"}})
		broken := newStep(t, &model.Step{ID: "broken", Name: "Broken", Number: 5, If: yaml.Node{Value: badExpression}})

		require.NoError(t, runStepExecutor(outer, stepStagePost, runStepExecutor(child, stepStageMain, noopExecutor))(ctx))
		require.NoError(t, runStepExecutor(skipped, stepStageMain, noopExecutor)(ctx))
		require.Error(t, runStepExecutor(broken, stepStageMain, noopExecutor)(ctx))

		ended := spans.Ended()
		require.Len(t, ended, 4)
		assert.Equal(t, []string{"Restore", "Post Run actions/cache@v4", "Deploy", "Broken"}, []string{ended[0].Name(), ended[1].Name(), ended[2].Name(), ended[3].Name()})
		assert.Equal(t, ended[1].SpanContext().SpanID(), ended[0].Parent().SpanID())
		assert.Contains(t, ended[0].Attributes(), semconv.CICDPipelineTaskRunID("7.3.post.1"))
		assert.Contains(t, ended[2].Attributes(), semconv.CICDPipelineTaskRunResultSkip)
		assert.Contains(t, ended[3].Attributes(), semconv.CICDPipelineTaskRunResultFailure)
	})

	t.Run("timeout must be positive", func(t *testing.T) {
		for timeout, wantDeadline := range map[string]bool{"-1": false, "0": false, "1": true} {
			ctx, cancel := evaluateStepTimeout(t.Context(), newStep(t, &model.Step{TimeoutMinutes: timeout}))
			_, hasDeadline := ctx.Deadline()
			cancel()
			assert.Equal(t, wantDeadline, hasDeadline, timeout)
		}
	})

	t.Run("timeout reads step environment", func(t *testing.T) {
		step := newStep(t, &model.Step{ID: "timeout", TimeoutMinutes: "${{ fromJSON(env.STEP_TIMEOUT) }}"})
		require.NoError(t, step.Step.Env.Encode(map[string]string{"STEP_TIMEOUT": "1"}))
		require.NoError(t, runStepExecutor(step, stepStageMain, func(ctx context.Context) error {
			_, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline)
			return nil
		})(t.Context()))
	})
}

func lastRecordAttr(h *logtest.Handler, key string) any {
	v, _ := h.LastRecord().Attr(key)
	return v
}
