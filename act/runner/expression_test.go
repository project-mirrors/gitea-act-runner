// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"strings"
	"testing"

	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	assert "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v4"
)

func createRunContext(t *testing.T) *RunContext {
	var yml yaml.Node
	err := yml.Encode(map[string][]any{
		"os":  {"Linux", "Windows"},
		"foo": {"bar", "baz"},
	})
	assert.NoError(t, err)

	return &RunContext{
		Config: &Config{
			Workdir: ".",
			Secrets: map[string]string{
				"CASE_INSENSITIVE_SECRET": "value",
			},
			Vars: map[string]string{
				"CASE_INSENSITIVE_VAR": "value",
			},
		},
		Env: map[string]string{
			"key": "value",
		},
		Run: &model.Run{
			JobID: "job1",
			Workflow: &model.Workflow{
				Name: "test-workflow",
				Jobs: map[string]*model.Job{
					"job1": {
						Strategy: &model.Strategy{
							RawMatrix: yml,
						},
					},
				},
			},
		},
		Matrix: map[string]any{
			"os":  "Linux",
			"foo": "bar",
		},
		StepResults: map[string]*model.StepResult{
			"idwithnothing": {
				Conclusion: model.StepStatusSuccess,
				Outcome:    model.StepStatusFailure,
				Outputs: map[string]string{
					"foowithnothing": "barwithnothing",
				},
			},
			"id-with-hyphens": {
				Conclusion: model.StepStatusSuccess,
				Outcome:    model.StepStatusFailure,
				Outputs: map[string]string{
					"foo-with-hyphens": "bar-with-hyphens",
				},
			},
			"id_with_underscores": {
				Conclusion: model.StepStatusSuccess,
				Outcome:    model.StepStatusFailure,
				Outputs: map[string]string{
					"foo_with_underscores": "bar_with_underscores",
				},
			},
		},
	}
}

func TestEvaluateRunContext(t *testing.T) {
	rc := createRunContext(t)
	ee := rc.NewExpressionEvaluator(context.Background())

	tables := []struct {
		in      string
		out     any
		errMesg string
	}{
		{" 1 ", 1.0, ""},
		// {"1 + 3", "4", ""},
		// {"(1 + 3) * -2", "-8", ""},
		{"'my text'", "my text", ""},
		{"contains('my text', 'te')", true, ""},
		{"contains('my TEXT', 'te')", true, ""},
		{"contains(fromJSON('[\"my text\"]'), 'te')", false, ""},
		{"contains(fromJSON('[\"foo\",\"bar\"]'), 'bar')", true, ""},
		{"startsWith('hello world', 'He')", true, ""},
		{"endsWith('hello world', 'ld')", true, ""},
		{"format('0:{0} 2:{2} 1:{1}', 'zero', 'one', 'two')", "0:zero 2:two 1:one", ""},
		{"join(fromJSON('[\"hello\"]'),'octocat')", "hello", ""},
		{"join(fromJSON('[\"hello\",\"mona\",\"the\"]'),'octocat')", "hellooctocatmonaoctocatthe", ""},
		{"join('hello','mona')", "hello", ""},
		{"toJSON(env)", "{\n  \"ACT\": \"true\",\n  \"ACT_SKIP_CHECKOUT\": \"true\",\n  \"key\": \"value\"\n}", ""},
		{"toJson(env)", "{\n  \"ACT\": \"true\",\n  \"ACT_SKIP_CHECKOUT\": \"true\",\n  \"key\": \"value\"\n}", ""},
		{"(fromJSON('{\"foo\":\"bar\"}')).foo", "bar", ""},
		{"(fromJson('{\"foo\":\"bar\"}')).foo", "bar", ""},
		{"(fromJson('[\"foo\",\"bar\"]'))[1]", "bar", ""},
		// github does return an empty string for non-existent files
		{"hashFiles('**/non-extant-files')", "", ""},
		{"hashFiles('**/non-extant-files', '**/more-non-extant-files')", "", ""},
		{"hashFiles('**/non.extant.files')", "", ""},
		{"hashFiles('**/non''extant''files')", "", ""},
		{"success()", true, ""},
		{"failure()", false, ""},
		{"always()", true, ""},
		{"cancelled()", false, ""},
		{"github.workflow", "test-workflow", ""},
		{"github.actor", "nektos/act", ""},
		{"github.run_id", "1", ""},
		{"github.run_number", "1", ""},
		{"job.status", "success", ""},
		{"matrix.os", "Linux", ""},
		{"matrix.foo", "bar", ""},
		{"env.key", "value", ""},
		{"secrets.CASE_INSENSITIVE_SECRET", "value", ""},
		{"secrets.case_insensitive_secret", "value", ""},
		{"vars.CASE_INSENSITIVE_VAR", "value", ""},
		{"vars.case_insensitive_var", "value", ""},
		{"format('{{0}}', 'test')", "{0}", ""},
		{"format('{{{0}}}', 'test')", "{test}", ""},
		{"format('}}')", "}", ""},
		{"format('echo Hello {0} ${{Test}}', 'World')", "echo Hello World ${Test}", ""},
		{"format('echo Hello {0} ${{Test}}', github.undefined_property)", "echo Hello  ${Test}", ""},
		{"format('echo Hello {0}{1} ${{Te{0}st}}', github.undefined_property, 'World')", "echo Hello World ${Test}", ""},
		{"format('{0}', '{1}', 'World')", "{1}", ""},
		{"format('{{{0}', '{1}', 'World')", "{{1}", ""},
	}

	for _, table := range tables {
		t.Run(table.in, func(t *testing.T) {
			assertObject := assert.New(t)
			out, err := ee.evaluate(context.Background(), table.in, exprparser.DefaultStatusCheckNone)
			if table.errMesg == "" {
				assertObject.NoError(err, table.in) //nolint:testifylint // pre-existing issue from nektos/act
				assertObject.Equal(table.out, out, table.in)
			} else {
				assertObject.Error(err, table.in) //nolint:testifylint // pre-existing issue from nektos/act
				assertObject.Equal(table.errMesg, err.Error(), table.in)
			}
		})
	}
}

func TestEvaluateStep(t *testing.T) {
	rc := createRunContext(t)
	rc.Env["INPUT_FORGED"] = "leaked"
	step := &stepRun{
		RunContext: rc,
		env:        map[string]string{"INPUT_FORGED": "leaked"},
	}

	ee := rc.NewStepExpressionEvaluator(context.Background(), step)

	tables := []struct {
		in      string
		out     any
		errMesg string
	}{
		{"steps.idwithnothing.conclusion", model.StepStatusSuccess.String(), ""},
		{"steps.idwithnothing.outcome", model.StepStatusFailure.String(), ""},
		{"steps.idwithnothing.outputs.foowithnothing", "barwithnothing", ""},
		{"steps.id-with-hyphens.conclusion", model.StepStatusSuccess.String(), ""},
		{"steps.id-with-hyphens.outcome", model.StepStatusFailure.String(), ""},
		{"steps.id-with-hyphens.outputs.foo-with-hyphens", "bar-with-hyphens", ""},
		{"steps.id_with_underscores.conclusion", model.StepStatusSuccess.String(), ""},
		{"steps.id_with_underscores.outcome", model.StepStatusFailure.String(), ""},
		{"steps.id_with_underscores.outputs.foo_with_underscores", "bar_with_underscores", ""},
		{"inputs.forged", nil, ""}, // INPUT_* env is not an input
	}

	for _, table := range tables {
		t.Run(table.in, func(t *testing.T) {
			assertObject := assert.New(t)
			out, err := ee.evaluate(context.Background(), table.in, exprparser.DefaultStatusCheckNone)
			if table.errMesg == "" {
				assertObject.NoError(err, table.in) //nolint:testifylint // pre-existing issue from nektos/act
				assertObject.Equal(table.out, out, table.in)
			} else {
				assertObject.Error(err, table.in) //nolint:testifylint // pre-existing issue from nektos/act
				assertObject.Equal(table.errMesg, err.Error(), table.in)
			}
		})
	}
}

func TestInterpolate(t *testing.T) {
	rc := &RunContext{
		Config: &Config{
			Workdir: ".",
			Secrets: map[string]string{
				"CASE_INSENSITIVE_SECRET": "value",
			},
			Vars: map[string]string{
				"CASE_INSENSITIVE_VAR": "value",
			},
		},
		Env: map[string]string{
			"KEYWITHNOTHING":       "valuewithnothing",
			"KEY-WITH-HYPHENS":     "value-with-hyphens",
			"KEY_WITH_UNDERSCORES": "value_with_underscores",
			"SOMETHING_TRUE":       "true",
			"SOMETHING_FALSE":      "false",
		},
		Run: &model.Run{
			JobID: "job1",
			Workflow: &model.Workflow{
				Name: "test-workflow",
				Jobs: map[string]*model.Job{
					"job1": {},
				},
			},
		},
	}
	ee := rc.NewExpressionEvaluator(context.Background())
	tables := []struct {
		in  string
		out string
	}{
		{" text ", " text "},
		{" $text ", " $text "},
		{" ${text} ", " ${text} "},
		{" ${{          1                         }} to ${{2}} ", " 1 to 2 "},
		{" ${{  (true || false)  }} to ${{2}} ", " true to 2 "},
		{" ${{  (false   ||  '}}'  )    }} to ${{2}} ", " }} to 2 "},
		{" ${{ env.KEYWITHNOTHING }} ", " valuewithnothing "},
		{" ${{ env.KEY-WITH-HYPHENS }} ", " value-with-hyphens "},
		{" ${{ env.KEY_WITH_UNDERSCORES }} ", " value_with_underscores "},
		{"${{ secrets.CASE_INSENSITIVE_SECRET }}", "value"},
		{"${{ secrets.case_insensitive_secret }}", "value"},
		{"${{ vars.CASE_INSENSITIVE_VAR }}", "value"},
		{"${{ vars.case_insensitive_var }}", "value"},
		{"${{ env.UNKNOWN }}", ""},
		{"${{ env.SOMETHING_TRUE }}", "true"},
		{"${{ env.SOMETHING_FALSE }}", "false"},
		{"${{ !env.SOMETHING_TRUE }}", "false"},
		{"${{ !env.SOMETHING_FALSE }}", "false"},
		{"${{ !env.SOMETHING_TRUE && true }}", "false"},
		{"${{ !env.SOMETHING_FALSE && true }}", "false"},
		{"${{ env.SOMETHING_TRUE && true }}", "true"},
		{"${{ env.SOMETHING_FALSE && true }}", "true"},
		{"${{ !env.SOMETHING_TRUE || true }}", "true"},
		{"${{ !env.SOMETHING_FALSE || true }}", "true"},
		{"${{ !env.SOMETHING_TRUE && false }}", "false"},
		{"${{ !env.SOMETHING_FALSE && false }}", "false"},
		{"${{ !env.SOMETHING_TRUE || false }}", "false"},
		{"${{ !env.SOMETHING_FALSE || false }}", "false"},
		{"${{ env.SOMETHING_TRUE || false }}", "true"},
		{"${{ env.SOMETHING_FALSE || false }}", "false"},
		{"${{ env.SOMETHING_FALSE }} && ${{ env.SOMETHING_TRUE }}", "false && true"},
		{"${{ fromJSON('{}') < 2 }}", "false"},
		{"${{ 1 }}", "1"},
		{"${{ 1.0 }}", "1"},
		{"${{ null }}", ""},
		{"${{ fromJSON('[1,2]') }}", "Array"},
		{"${{ fromJSON('{\"a\":1}') }}", "Object"},
	}

	for _, table := range tables {
		t.Run("interpolate", func(t *testing.T) {
			out, err := ee.Interpolate(context.Background(), table.in)
			require.NoError(t, err, table.in)
			assert.Equal(t, table.out, out, table.in)
		})
	}

	for _, in := range []string{"${{ 1) && (2 }}", "run ${{ 1) && (2 }} now", "${{ 1"} {
		_, err := ee.Interpolate(context.Background(), in)
		assert.Error(t, err, in)
	}
}

func TestGetEvaluatorInputsBoolean(t *testing.T) {
	workflows := map[string]string{
		"workflow_call": `
on:
  workflow_call:
    inputs:
      flag:
        type: boolean
        default: true
      name:
        type: string
        default: gitea
      unset:
        type: boolean
`,
		"workflow_dispatch": `
on:
  workflow_dispatch:
    inputs:
      flag:
        type: boolean
        default: true
      name:
        type: string
        default: gitea
      unset:
        type: boolean
`,
	}

	tables := []struct {
		name  string
		event map[string]any
		flag  any
	}{
		{
			// Gitea >= 1.27 resolves the inputs server-side and sends native JSON types
			name:  "native bool true",
			event: map[string]any{"inputs": map[string]any{"flag": true}},
			flag:  true,
		},
		{
			name:  "native bool false",
			event: map[string]any{"inputs": map[string]any{"flag": false}},
			flag:  false,
		},
		{
			name:  "string true",
			event: map[string]any{"inputs": map[string]any{"flag": "true"}},
			flag:  true,
		},
		{
			name:  "string false",
			event: map[string]any{"inputs": map[string]any{"flag": "false"}},
			flag:  false,
		},
		{
			name:  "default is used when the event carries no inputs",
			event: map[string]any{},
			flag:  true,
		},
	}

	for eventName, workflow := range workflows {
		for _, table := range tables {
			t.Run(eventName+"/"+table.name, func(t *testing.T) {
				wf, err := model.ReadWorkflow(strings.NewReader(workflow))
				require.NoError(t, err)

				rc := &RunContext{
					Config: &Config{Workdir: "."},
					Run:    &model.Run{JobID: "job1", Workflow: wf},
				}
				ghc := &model.GithubContext{EventName: eventName, Event: table.event}

				inputs := getEvaluatorInputs(rc, nil, ghc)
				assert.Equal(t, table.flag, inputs["flag"])
				assert.Equal(t, "gitea", inputs["name"])
				assert.Equal(t, false, inputs["unset"])
			})
		}
	}

	t.Run("deferred workflow call inputs", func(t *testing.T) {
		workflow, err := model.ReadWorkflow(strings.NewReader(workflows["workflow_call"] + `
jobs:
  test: {}
  call:
    uses: ./reuse.yml
    with: ${{ fromJSON(matrix.args) }}
`))
		require.NoError(t, err)
		workflow.GetJob("test")
		job := workflow.GetJob("call")
		rawWith := model.CloneYamlNode(job.RawWith)
		for _, value := range []string{"true", "false"} {
			t.Run(value, func(t *testing.T) {
				t.Parallel()
				parent, err := (&runnerImpl{config: &Config{}}).newRunContext(t.Context(), &model.Run{Workflow: workflow, JobID: "call"}, map[string]any{"args": `{"flag":` + value + `,"name":"${{ 'runner' }}"}`})
				require.NoError(t, err)
				child, err := (&runnerImpl{config: &Config{}, caller: &caller{runContext: parent}}).newRunContext(t.Context(), &model.Run{Workflow: workflow, JobID: "test"}, nil)
				require.NoError(t, err)
				assert.Equal(t, map[string]any{"flag": value == "true", "name": "${{ 'runner' }}", "unset": false}, child.workflowCallInputs)
				assert.Equal(t, rawWith, job.RawWith)
				assert.Nil(t, job.With)
			})
		}
	})
}

func TestResolveWorkflowCallToleratesMismatchesAndPassesInheritedSecretsVerbatim(t *testing.T) {
	callerWorkflow, err := model.ReadWorkflow(strings.NewReader(`
jobs:
  call:
    uses: ./reuse.yml
    with: {flag: not-a-boolean}
    secrets: {undeclared: "${{ secrets.token }}"}
  inherit:
    uses: ./reuse.yml
    secrets: inherit
`))
	require.NoError(t, err)
	callee, err := model.ReadWorkflow(strings.NewReader(`
on:
  workflow_call:
    inputs:
      flag: {type: boolean, default: true}
jobs:
  test: {}
`))
	require.NoError(t, err)
	notCallable, err := model.ReadWorkflow(strings.NewReader("on: push\njobs:\n  test: {}\n"))
	require.NoError(t, err)
	config := &Config{Secrets: map[string]string{"token": "${{ github.token }}"}}
	resolve := func(callerJobID string, workflow *model.Workflow) *RunContext {
		parent, err := (&runnerImpl{config: config}).newRunContext(t.Context(), &model.Run{Workflow: callerWorkflow, JobID: callerJobID}, nil)
		require.NoError(t, err)
		child, err := (&runnerImpl{config: config, caller: &caller{runContext: parent}}).newRunContext(t.Context(), &model.Run{Workflow: workflow, JobID: "test"}, nil)
		require.NoError(t, err)
		return child
	}

	called := resolve("call", callee)
	assert.Equal(t, map[string]any{"flag": true}, called.workflowCallInputs)
	assert.Equal(t, map[string]string{"undeclared": "${{ github.token }}"}, called.workflowCallSecrets)
	inherited := resolve("inherit", notCallable)
	assert.Empty(t, inherited.workflowCallInputs)
	assert.Equal(t, config.Secrets, inherited.workflowCallSecrets)
}

func TestJobNameMasksSecrets(t *testing.T) {
	workflow, err := model.ReadWorkflow(strings.NewReader(`
jobs:
  a:
    name: deploy ${{ secrets.A }}
  b:
    name: deploy ${{ secrets.B }}
`))
	require.NoError(t, err)

	runner := &runnerImpl{config: &Config{Secrets: map[string]string{"A": "s3cr3t-a", "B": "s3cr3t-b"}}}
	containerName := func(jobID string) string {
		rc, err := runner.newRunContext(t.Context(), &model.Run{JobID: jobID, Workflow: workflow}, nil)
		require.NoError(t, err)
		assert.NotContains(t, rc.Name, "s3cr3t")
		return rc.jobContainerName()
	}

	a, b := containerName("a"), containerName("b")
	assert.NotContains(t, a, "s3cr3t") // it reaches the container name, which no log masker covers
	assert.NotEqual(t, a, b)           // masking the name must not collapse two jobs onto one container
}
