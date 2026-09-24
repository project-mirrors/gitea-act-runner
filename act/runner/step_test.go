// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/telemetry"

	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	log "github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
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

func (sm *stepMock) getGithubContext(ctx context.Context) *model.GithubContext {
	args := sm.Called()
	return args.Get(0).(*RunContext).getGithubContext(ctx)
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
	cm := &containerMock{}
	sm := &stepMock{}

	rc := &RunContext{
		Config: &Config{
			Env: map[string]string{
				"GITHUB_RUN_ID": "runId",
			},
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
		JobContainer: cm,
	}
	step := &model.Step{}
	require.NoError(t, step.RawWith.Encode(map[string]string{"STEP_WITH": "with-value", "ID": "${{ fromJSON('1234567') }}"}))
	env := map[string]string{}

	sm.On("getRunContext").Return(rc)
	sm.On("getGithubContext").Return(rc)
	sm.On("getStepModel").Return(step)
	sm.On("getEnv").Return(&env)

	require.NoError(t, setupEnv(context.Background(), sm))
	require.NoError(t, setupInputs(context.Background(), sm))

	// These are commit or system specific
	delete(env, "GITHUB_REF")
	delete(env, "GITHUB_REF_NAME")
	delete(env, "GITHUB_REF_TYPE")
	delete(env, "GITHUB_SHA")
	delete(env, "GITHUB_WORKSPACE")
	delete(env, "GITHUB_REPOSITORY")
	delete(env, "GITHUB_REPOSITORY_OWNER")
	delete(env, "GITHUB_ACTOR")
	// Host-dependent, asserted in TestRunContextWithGithubEnvRunnerValues instead.
	delete(env, "RUNNER_NAME")
	delete(env, "RUNNER_WORKSPACE")

	assert.Equal(t, map[string]string{
		"ACT":                      "true",
		"ACT_SKIP_CHECKOUT":        "true",
		"CI":                       "true",
		"GITHUB_ACTION":            "",
		"GITHUB_ACTIONS":           "true",
		"GITHUB_ACTION_PATH":       "",
		"GITHUB_ACTION_REF":        "",
		"GITHUB_ACTION_REPOSITORY": "",
		"GITHUB_API_URL":           "https:///api/v1", // Gitea uses api/v1 (upstream GitHub: api/v3)
		"GITHUB_BASE_REF":          "",
		"GITHUB_EVENT_NAME":        "",
		"GITHUB_EVENT_PATH":        "/var/run/act/workflow/event.json",
		"GITHUB_GRAPHQL_URL":       "",
		"GITHUB_HEAD_REF":          "",
		"GITHUB_JOB":               "1",
		"GITHUB_RETENTION_DAYS":    "0",
		"GITHUB_RUN_ATTEMPT":       "",
		"GITHUB_RUN_ID":            "runId",
		"GITHUB_RUN_NUMBER":        "1",
		"GITHUB_SERVER_URL":        "https://",
		"GITHUB_WORKFLOW":          "",
		"INPUT_ID":                 "1234567",
		"INPUT_STEP_WITH":          "with-value",
		"RC_KEY":                   "rcvalue",
		"RUNNER_ENVIRONMENT":       "self-hosted",
		"RUNNER_PERFLOG":           "/dev/null",
		"RUNNER_TRACKING_ID":       "",
	}, env)

	cm.AssertExpectations(t)

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
					rc, err := (&runnerImpl{config: &Config{}}).newRunContext(t.Context(), &model.Run{Workflow: workflow, JobID: "test"}, map[string]any{"env": `{"WORKFLOW":"` + value + `${{ github.job }}","PRIORITY":"workflow"}`, "priority": "${{ github.job }}", "defaults": `{"shell":"` + value + `"}`})
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
		rc, err := (&runnerImpl{config: &Config{}}).newRunContext(t.Context(), &model.Run{Workflow: workflow, JobID: "test"}, nil)
		require.NoError(t, err)
		rc.workflowCallInputs = map[string]any{"args": "${{ inputs.unresolved }}"}
		require.ErrorContains(t, setupInputs(t.Context(), &stepRun{RunContext: rc, Step: workflow.Jobs["test"].Steps[0].Clone(), env: map[string]string{}}), "with:")
	})
}

func TestIsStepEnabled(t *testing.T) {
	createTestStep := func(t *testing.T, input string) step {
		var step *model.Step
		err := yaml.Unmarshal([]byte(input), &step)
		assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act

		return &stepRun{
			RunContext: &RunContext{
				Config:      &Config{Workdir: ".", PlatformPicker: func([]string) string { return "ubuntu-latest" }},
				StepResults: map[string]*model.StepResult{},
				Env:         map[string]string{},
				Run: &model.Run{
					JobID: "job1",
					Workflow: &model.Workflow{
						Name: "workflow1",
						Jobs: map[string]*model.Job{
							"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
						},
					},
				},
			},
			Step: step,
		}
	}

	log.SetLevel(log.DebugLevel)
	assertObject := assert.New(t)

	// success()
	step := createTestStep(t, "if: success()")
	assertObject.True(isStepEnabled(context.Background(), step.getIfExpression(context.Background(), stepStageMain), step, stepStageMain))

	step = createTestStep(t, "if: success()")
	step.getRunContext().StepResults["a"] = &model.StepResult{
		Conclusion: model.StepStatusSuccess,
	}
	assertObject.True(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))

	step = createTestStep(t, "if: success()")
	step.getRunContext().StepResults["a"] = &model.StepResult{
		Conclusion: model.StepStatusFailure,
	}
	assertObject.False(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))

	// failure()
	step = createTestStep(t, "if: failure()")
	assertObject.False(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))

	step = createTestStep(t, "if: failure()")
	step.getRunContext().StepResults["a"] = &model.StepResult{
		Conclusion: model.StepStatusSuccess,
	}
	assertObject.False(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))

	step = createTestStep(t, "if: failure()")
	step.getRunContext().StepResults["a"] = &model.StepResult{
		Conclusion: model.StepStatusFailure,
	}
	assertObject.True(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))

	// always()
	step = createTestStep(t, "if: always()")
	assertObject.True(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))

	step = createTestStep(t, "if: always()")
	step.getRunContext().StepResults["a"] = &model.StepResult{
		Conclusion: model.StepStatusSuccess,
	}
	assertObject.True(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))

	step = createTestStep(t, "if: always()")
	step.getRunContext().StepResults["a"] = &model.StepResult{
		Conclusion: model.StepStatusFailure,
	}
	assertObject.True(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))

	// neither env nor the step's own with: values are inputs, at any stage
	step = createTestStep(t, "if: inputs.forged")
	step.getRunContext().Env["INPUT_FORGED"] = "leaked"
	*step.getEnv() = map[string]string{"INPUT_FORGED": "leaked"}
	assertObject.False(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStageMain))
	assertObject.False(isStepEnabled(context.Background(), step.getStepModel().If.Value, step, stepStagePost))
}

func TestIsContinueOnError(t *testing.T) {
	createTestStep := func(t *testing.T, input string) step {
		var step *model.Step
		err := yaml.Unmarshal([]byte(input), &step)
		assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act

		return &stepRun{
			RunContext: &RunContext{
				Config:      &Config{Workdir: ".", PlatformPicker: func([]string) string { return "ubuntu-latest" }},
				StepResults: map[string]*model.StepResult{},
				Env:         map[string]string{},
				Run: &model.Run{
					JobID: "job1",
					Workflow: &model.Workflow{
						Name: "workflow1",
						Jobs: map[string]*model.Job{
							"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
						},
					},
				},
			},
			Step: step,
		}
	}

	log.SetLevel(log.DebugLevel)
	assertObject := assert.New(t)

	// absent
	step := createTestStep(t, "name: test")
	continueOnError, err := isContinueOnError(context.Background(), step.getStepModel().RawContinueOnError, step, stepStageMain)
	assertObject.False(continueOnError)
	assertObject.NoError(err) //nolint:testifylint // pre-existing issue from nektos/act

	// explcit true
	step = createTestStep(t, "continue-on-error: true")
	continueOnError, err = isContinueOnError(context.Background(), step.getStepModel().RawContinueOnError, step, stepStageMain)
	assertObject.True(continueOnError)
	assertObject.NoError(err) //nolint:testifylint // pre-existing issue from nektos/act

	// explicit false
	step = createTestStep(t, "continue-on-error: false")
	continueOnError, err = isContinueOnError(context.Background(), step.getStepModel().RawContinueOnError, step, stepStageMain)
	assertObject.False(continueOnError)
	assertObject.NoError(err) //nolint:testifylint // pre-existing issue from nektos/act

	// expression true
	step = createTestStep(t, "continue-on-error: ${{ 'test' == 'test' }}")
	continueOnError, err = isContinueOnError(context.Background(), step.getStepModel().RawContinueOnError, step, stepStageMain)
	assertObject.True(continueOnError)
	assertObject.NoError(err) //nolint:testifylint // pre-existing issue from nektos/act

	// expression false
	step = createTestStep(t, "continue-on-error: ${{ 'test' != 'test' }}")
	continueOnError, err = isContinueOnError(context.Background(), step.getStepModel().RawContinueOnError, step, stepStageMain)
	assertObject.False(continueOnError)
	assertObject.NoError(err) //nolint:testifylint // pre-existing issue from nektos/act

	// the step's own with: values are not inputs
	step = createTestStep(t, "continue-on-error: ${{ inputs.forged }}")
	*step.getEnv() = map[string]string{"INPUT_FORGED": "true"}
	continueOnError, err = isContinueOnError(context.Background(), step.getStepModel().RawContinueOnError, step, stepStageMain)
	assertObject.False(continueOnError)
	require.NoError(t, err)

	// expression parse error
	step = createTestStep(t, "continue-on-error: ${{ 'test' != test }}")
	continueOnError, err = isContinueOnError(context.Background(), step.getStepModel().RawContinueOnError, step, stepStageMain)
	assertObject.False(continueOnError)
	assertObject.Error(err)
}

// A refused ::set-env::/::add-path:: records a job-scoped error. When the step that
// produced it also fails on its own, the refusal must be cleared at the step boundary, so
// it fails only that step and never leaks onto a later step that runs anyway (if: always()).
func TestRunStepExecutorDoesNotLeakRefusalToNextStep(t *testing.T) {
	cm := &containerMock{}
	noop := func(context.Context) error { return nil }
	cm.On("Copy", mock.Anything, mock.Anything).Return(noop)
	cm.On("UpdateFromEnv", mock.Anything, mock.Anything).Return(noop)

	rc := &RunContext{
		Config: &Config{Env: map[string]string{}},
		Run: &model.Run{
			JobID:    "1",
			Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}},
		},
		Env:          map[string]string{},
		StepResults:  map[string]*model.StepResult{},
		JobContainer: cm,
	}
	rc.ExprEval = rc.NewExpressionEvaluator(context.Background())
	// Dryrun skips reading the path file back from the (mocked) container.
	ctx := common.WithDryrun(context.Background(), true)

	// A refusal parsed out of the job container's own output belongs to no step, so the
	// first step must not be failed by it.
	rc.commandHandler(ctx)("::set-env name=setup::y\n")
	stepSetup := &stepRun{RunContext: rc, Step: &model.Step{ID: "setup"}, env: map[string]string{}}
	require.NoError(t, runStepExecutor(stepSetup, stepStageMain, func(context.Context) error { return nil })(ctx))

	// Step A refuses a ::set-env:: and then fails on its own.
	stepA := &stepRun{RunContext: rc, Step: &model.Step{ID: "a"}, env: map[string]string{}}
	errA := runStepExecutor(stepA, stepStageMain, func(context.Context) error {
		rc.commandHandler(ctx)("::set-env name=x::y\n")
		return errors.New("boom")
	})(ctx)
	// The step fails with its own error, not the refusal.
	require.ErrorContains(t, errA, "boom")

	// Step B runs despite step A's failure (if: always()) and issues no unsecure command;
	// it must not inherit step A's refusal.
	stepB := &stepRun{RunContext: rc, Step: &model.Step{ID: "b", If: yaml.Node{Value: "always()"}}, env: map[string]string{}}
	errB := runStepExecutor(stepB, stepStageMain, func(context.Context) error { return nil })(ctx)
	require.NoError(t, errB)
}

func TestRunStepExecutorParity(t *testing.T) {
	newStep := func(t *testing.T, stepModel *model.Step) *stepRun {
		rc := createRunContext(t)
		rc.JobContainer = &container.HostEnvironment{ActPath: t.TempDir()}
		rc.ExprEval = rc.NewExpressionEvaluator(context.Background())
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
			logger, hook := logrustest.NewNullLogger()
			err := runStepExecutor(step, stepStageMain, test.executor)(common.WithLogger(context.Background(), logger))

			require.ErrorContains(t, err, test.wantError)
			assert.Equal(t, model.StepStatusFailure, step.RunContext.StepResults[test.step.ID].Conclusion)
			assert.Equal(t, model.StepStatusFailure, hook.LastEntry().Data["stepResult"])
		})
	}

	t.Run("file command error honors continue-on-error", func(t *testing.T) {
		step := newStep(t, &model.Step{ID: "commands", RawContinueOnError: "true"})
		logger, hook := logrustest.NewNullLogger()
		err := runStepExecutor(step, stepStageMain, func(context.Context) error {
			require.NoError(t, os.WriteFile(step.env["GITHUB_ENV"], []byte("GOOD=1\nmalformed\n"), 0o600))
			require.NoError(t, os.WriteFile(step.env["GITHUB_OUTPUT"], []byte("kept=value\n"), 0o600))
			require.NoError(t, os.WriteFile(step.env["GITHUB_STATE"], []byte("saved=value\n"), 0o600))
			return nil
		})(common.WithLogger(context.Background(), logger))

		require.NoError(t, err)
		result := step.RunContext.StepResults[step.Step.ID]
		assert.Equal(t, model.StepStatusFailure, result.Outcome)
		assert.Equal(t, model.StepStatusSuccess, result.Conclusion)
		assert.Equal(t, model.StepStatusSuccess, hook.LastEntry().Data["stepResult"])
		assert.Equal(t, "1", step.RunContext.Env["GOOD"])
		assert.Equal(t, "value", result.Outputs["kept"])
		assert.Equal(t, "value", step.RunContext.IntraActionState[step.Step.ID]["saved"])
		for _, name := range []string{"GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_STATE", "GITHUB_PATH"} {
			contents, readErr := os.ReadFile(step.env[name])
			require.NoError(t, readErr)
			assert.Empty(t, contents)
		}
	})

	t.Run("composite child files do not leak", func(t *testing.T) {
		outer := newStep(t, &model.Step{ID: "outer"})
		childRC := &RunContext{
			Config: outer.RunContext.Config, Run: outer.RunContext.Run, Env: map[string]string{}, StepResults: map[string]*model.StepResult{},
			JobContainer: outer.RunContext.JobContainer, Parent: outer.RunContext,
		}
		childRC.ExprEval = childRC.NewExpressionEvaluator(context.Background())
		child := &stepRun{RunContext: childRC, Step: &model.Step{ID: "child"}, env: map[string]string{}}

		err := runStepExecutor(outer, stepStageMain, func(ctx context.Context) error {
			require.NoError(t, runStepExecutor(child, stepStageMain, func(context.Context) error {
				require.NoError(t, os.WriteFile(child.env["GITHUB_OUTPUT"], []byte("declared=child\nundeclared=leak\n"), 0o600))
				require.NoError(t, os.WriteFile(child.env["GITHUB_STATE"], []byte("saved=child\n"), 0o600))
				return nil
			})(ctx))
			outer.RunContext.setOutput(ctx, map[string]string{"name": "declared"}, "outer")
			return nil
		})(context.Background())

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
		exprEval := createRunContext(t).NewExpressionEvaluator(context.Background())
		for timeout, wantDeadline := range map[string]bool{"-1": false, "0": false, "1": true} {
			ctx, cancel := evaluateStepTimeout(context.Background(), exprEval, &model.Step{TimeoutMinutes: timeout})
			_, hasDeadline := ctx.Deadline()
			cancel()
			assert.Equal(t, wantDeadline, hasDeadline, timeout)
		}
	})
}
