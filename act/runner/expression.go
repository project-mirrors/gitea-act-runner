// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	_ "embed"

	"gitea.dev/actionslib/pkg/expreval"
	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"go.yaml.in/yaml/v4"
)

// setStrategyContext leaves `max-parallel` unset when the job declares none, as GitHub does.
func setStrategyContext(strategy map[string]any, jobStrategy *model.Strategy) {
	strategy["fail-fast"] = jobStrategy.GetFailFast()
	if limit, declared, err := jobStrategy.ParseMaxParallel(); declared && err == nil {
		strategy["max-parallel"] = limit
	}
}

// decodeDeferred decodes a whole-value `${{ }}` parked in a Raw* field, evaluating a clone since the node is shared across matrix combinations.
func decodeDeferred[T any](ctx context.Context, eval *expressionEvaluator, name string, raw yaml.Node, out *T) error {
	if raw.Kind != yaml.ScalarNode {
		return nil
	}
	node := model.CloneYamlNode(raw)
	if err := eval.EvaluateYamlNode(ctx, &node); err != nil {
		return fmt.Errorf("unable to evaluate %s: %w", name, err)
	}
	if err := node.Decode(out); err != nil {
		return fmt.Errorf("unable to decode %s: %w", name, err)
	}
	return nil
}

// NewExpressionEvaluator creates a new evaluator
func (rc *RunContext) NewExpressionEvaluator(ctx context.Context) *ExpressionEvaluator {
	return rc.NewExpressionEvaluatorWithEnv(ctx, rc.GetEnv())
}

func (rc *RunContext) NewExpressionEvaluatorWithEnv(ctx context.Context, env map[string]string) *ExpressionEvaluator {
	var workflowCallResult map[string]*model.WorkflowCallResult

	// todo: cleanup EvaluationEnvironment creation
	using := make(map[string]exprparser.Needs)
	strategy := make(map[string]any)
	if rc.Run != nil {
		job := rc.Run.Job()
		if job != nil && job.Strategy != nil {
			setStrategyContext(strategy, job.Strategy)
		}

		jobs := rc.Run.Workflow.Jobs
		jobNeeds := rc.Run.Job().Needs()

		for _, needs := range jobNeeds {
			using[needs] = exprparser.Needs{
				Outputs: jobs[needs].Outputs,
				Result:  jobs[needs].NeedsResult(),
			}
		}

		// only setup jobs context in case of workflow_call
		// and existing expression evaluator (this means, jobs are at
		// least ready to run)
		if rc.caller != nil && rc.ExprEval != nil {
			workflowCallResult = map[string]*model.WorkflowCallResult{}

			for jobName, job := range jobs {
				result := model.WorkflowCallResult{
					Outputs: map[string]string{},
				}
				maps.Copy(result.Outputs, job.Outputs)
				workflowCallResult[jobName] = &result
			}
		}
	}

	ghc := rc.getGithubContext(ctx)
	inputs := getEvaluatorInputs(rc, rc.actionInputs, ghc)

	ee := &exprparser.EvaluationEnvironment{
		Github: ghc,
		Env:    env,
		Job:    rc.getJobContext(),
		Jobs:   &workflowCallResult,
		// todo: should be unavailable
		// but required to interpolate/evaluate the step outputs on the job
		Steps:     rc.getStepsContext(),
		Secrets:   getWorkflowSecrets(rc),
		Vars:      getWorkflowVars(ctx, rc),
		Strategy:  strategy,
		Matrix:    rc.Matrix,
		Needs:     using,
		Inputs:    inputs,
		HashFiles: getHashFilesFunction(ctx, rc),
	}
	ee.Runner = rc.getRunnerContext(ctx)
	return &expressionEvaluator{
		interpreter: exprparser.NewInterpeter(ee, exprparser.Config{
			Run:        rc.Run,
			WorkingDir: rc.Config.Workdir,
			Context:    "job",
		}),
	}
}

//go:embed hashfiles/index.js
var hashfiles string

// NewStepExpressionEvaluator creates a new evaluator with the `inputs` of the enclosing workflow or composite action
func (rc *RunContext) NewStepExpressionEvaluator(ctx context.Context, step step) *ExpressionEvaluator {
	return rc.newStepExpressionEvaluator(ctx, step, rc.actionInputs)
}

// NewActionInputsExpressionEvaluator creates a new evaluator with the step's own with: values as `inputs`
func (rc *RunContext) NewActionInputsExpressionEvaluator(ctx context.Context, step step) *ExpressionEvaluator {
	return rc.newStepExpressionEvaluator(ctx, step, inputsFromEnv(*step.getEnv()))
}

func (rc *RunContext) newStepExpressionEvaluator(ctx context.Context, step step, stepInputs map[string]any) *ExpressionEvaluator {
	// todo: cleanup EvaluationEnvironment creation
	job := rc.Run.Job()
	strategy := make(map[string]any)
	if job.Strategy != nil {
		setStrategyContext(strategy, job.Strategy)
	}

	jobs := rc.Run.Workflow.Jobs
	jobNeeds := rc.Run.Job().Needs()

	using := make(map[string]exprparser.Needs)
	for _, needs := range jobNeeds {
		using[needs] = exprparser.Needs{
			Outputs: jobs[needs].Outputs,
			Result:  jobs[needs].NeedsResult(),
		}
	}

	ee := &exprparser.EvaluationEnvironment{
		Github:   step.getGithubContext(ctx),
		Env:      *step.getEnv(),
		Job:      rc.getJobContext(),
		Steps:    rc.getStepsContext(),
		Secrets:  getWorkflowSecrets(rc),
		Vars:     getWorkflowVars(ctx, rc),
		Strategy: strategy,
		Matrix:   rc.Matrix,
		Needs:    using,
		// todo: should be unavailable
		// but required to interpolate/evaluate the inputs in actions/composite
		Inputs:    getEvaluatorInputs(rc, stepInputs, rc.getGithubContext(ctx)),
		HashFiles: getHashFilesFunction(ctx, rc),
	}
	ee.Runner = rc.getRunnerContext(ctx)
	return &expressionEvaluator{
		interpreter: exprparser.NewInterpeter(ee, exprparser.Config{
			Run:        rc.Run,
			WorkingDir: rc.Config.Workdir,
			Context:    "step",
		}),
	}
}

func getHashFilesFunction(ctx context.Context, rc *RunContext) func(v []reflect.Value) (any, error) {
	hashFiles := func(v []reflect.Value) (any, error) {
		if rc.JobContainer != nil {
			timeed, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			name := "workflow/hashfiles/index.js"
			hout := &bytes.Buffer{}
			herr := &bytes.Buffer{}
			patterns := []string{}
			followSymlink := false

			for i, p := range v {
				s := p.String()
				if i == 0 {
					if strings.HasPrefix(s, "--") {
						if strings.EqualFold(s, "--follow-symbolic-links") {
							followSymlink = true
							continue
						}
						return "", fmt.Errorf("invalid glob option %s, available option: '--follow-symbolic-links'", s)
					}
				}
				patterns = append(patterns, s)
			}
			env := map[string]string{}
			maps.Copy(env, rc.Env)
			env["patterns"] = strings.Join(patterns, "\n")
			if followSymlink {
				env["followSymbolicLinks"] = "true"
			}

			stdout, stderr := rc.JobContainer.ReplaceLogWriter(hout, herr)
			_ = rc.JobContainer.Copy(rc.JobContainer.GetActPath(), &container.FileEntry{
				Name: name,
				Mode: 0o644,
				Body: hashfiles,
			}).
				Then(rc.JobContainer.Exec([]string{"node", path.Join(rc.JobContainer.GetActPath(), name)},
					env, "", "")).
				Finally(func(context.Context) error {
					rc.JobContainer.ReplaceLogWriter(stdout, stderr)
					return nil
				})(timeed)
			output := hout.String() + "\n" + herr.String()
			guard := "__OUTPUT__"
			outstart := strings.Index(output, guard)
			if outstart != -1 {
				outstart += len(guard)
				outend := strings.Index(output[outstart:], guard)
				if outend != -1 {
					return output[outstart : outstart+outend], nil
				}
			}
		}
		return "", nil
	}
	return hashFiles
}

type expressionEvaluator struct {
	interpreter exprparser.Interpreter
}

type ExpressionEvaluator = expressionEvaluator

func (ee expressionEvaluator) evaluate(ctx context.Context, in string, defaultStatusCheck exprparser.DefaultStatusCheck) (any, error) {
	logger := common.Logger(ctx)
	logger.Debugf("evaluating expression '%s'", in)
	evaluated, err := ee.interpreter.Evaluate(in, defaultStatusCheck)

	// evaluated is an any: %t renders everything but a bool as "%!t(string=...)"
	printable := regexp.MustCompile(`::add-mask::.*`).ReplaceAllString(fmt.Sprintf("%v", evaluated), "::add-mask::***)")
	logger.Debugf("expression '%s' evaluated to '%s'", in, printable)

	return evaluated, err
}

// shared returns the evaluation layer of the shared library, bound to this context so the
// evaluation of every single expression is still logged and masked here.
func (ee expressionEvaluator) shared(ctx context.Context) expreval.Evaluator {
	return expreval.New(func(in string, defaultStatusCheck exprparser.DefaultStatusCheck) (any, error) {
		return ee.evaluate(ctx, in, defaultStatusCheck)
	})
}

func (ee expressionEvaluator) EvaluateYamlNode(ctx context.Context, node *yaml.Node) error {
	return ee.shared(ctx).EvaluateYamlNode(node)
}

func (ee expressionEvaluator) Interpolate(ctx context.Context, in string) (string, error) {
	return ee.shared(ctx).Interpolate(in)
}

// InterpolateName keeps the source text of a job or step name that cannot be evaluated, as GitHub does.
func (ee expressionEvaluator) InterpolateName(ctx context.Context, in string) string {
	out, err := ee.Interpolate(ctx, in)
	if err != nil {
		common.Logger(ctx).Warnf("Unable to evaluate the display name '%s': %s", in, err)
		return in
	}
	return out
}

// EvalBool evaluates an expression against given evaluator. An `if:` is an expression even without
// `${{ }}`, while literal text around one makes the whole value a string.
func EvalBool(ctx context.Context, evaluator *expressionEvaluator, expr string, defaultStatusCheck exprparser.DefaultStatusCheck) (bool, error) {
	return expreval.New(func(in string, dsc exprparser.DefaultStatusCheck) (any, error) {
		return evaluator.evaluate(ctx, in, dsc)
	}).EvalBool(expr, defaultStatusCheck)
}

func inputsFromEnv(env map[string]string) map[string]any {
	inputs := map[string]any{}
	for k, v := range env {
		if after, ok := strings.CutPrefix(k, "INPUT_"); ok {
			inputs[strings.ToLower(after)] = v
		}
	}
	return inputs
}

func getEvaluatorInputs(rc *RunContext, stepInputs map[string]any, ghc *model.GithubContext) map[string]any {
	inputs := map[string]any{}

	maps.Copy(inputs, rc.workflowCallInputs)
	maps.Copy(inputs, stepInputs)

	if ghc.EventName == "workflow_dispatch" {
		config := rc.Run.Workflow.WorkflowDispatchConfig()
		if config != nil && config.Inputs != nil {
			for k, v := range config.Inputs {
				value := nestedMapLookup(ghc.Event, "inputs", k)
				if value == nil {
					value = v.Default
				}
				inputs[k] = coerceInputValue(value, v.Type)
			}
		}
	}

	if ghc.EventName == "workflow_call" {
		config := rc.Run.Workflow.WorkflowCallConfig()
		if config != nil && config.Inputs != nil {
			for k, v := range config.Inputs {
				value := nestedMapLookup(ghc.Event, "inputs", k)
				if value == nil {
					value = v.Default
				}
				inputs[k] = coerceInputValue(value, v.Type)
			}
		}
	}
	return inputs
}

// coerceInputValue converts an input value to the type declared by the workflow.
// The event payload carries natively typed JSON values on newer Gitea versions,
// while defaults and older servers provide strings.
func coerceInputValue(value any, inputType string) any {
	if inputType != "boolean" {
		return value
	}
	if b, ok := value.(bool); ok {
		return b
	}
	return value == "true"
}

// resolveWorkflowCall evaluates the caller's with: and secrets: once, as the server does before dispatching a called workflow.
func (rc *RunContext) resolveWorkflowCall(ctx context.Context) error {
	if rc.caller == nil {
		return nil
	}
	callerJob := rc.caller.runContext.Run.Job()
	callerEval := rc.caller.runContext.ExprEval
	callerWith := callerJob.With
	if err := decodeDeferred(ctx, callerEval, "workflow inputs", callerJob.RawWith, &callerWith); err != nil {
		return err
	}
	calleeEval := sync.OnceValue(func() *expressionEvaluator { return rc.NewExpressionEvaluator(ctx) })
	config := rc.Run.Workflow.WorkflowCallConfig()

	rc.workflowCallInputs = make(map[string]any, len(config.Inputs))
	for name, input := range config.Inputs {
		value, eval, label := callerWith[name], callerEval, "input"
		if value == nil {
			value, eval, label = input.Default, calleeEval(), "the default of input"
		}
		if str, ok := value.(string); ok {
			var err error
			if value, err = eval.Interpolate(ctx, str); err != nil {
				return fmt.Errorf("unable to interpolate %s %s: %w", label, name, err)
			}
		}
		rc.workflowCallInputs[name] = coerceInputValue(value, input.Type)
	}

	secrets := callerJob.Secrets()
	if secrets == nil && callerJob.InheritSecrets() {
		secrets = rc.caller.runContext.Config.Secrets
	}
	rc.workflowCallSecrets = make(map[string]string, len(secrets))
	for k, v := range secrets {
		var err error
		if rc.workflowCallSecrets[k], err = callerEval.Interpolate(ctx, v); err != nil {
			return fmt.Errorf("unable to interpolate secret %s: %w", k, err)
		}
	}
	return nil
}

func getWorkflowSecrets(rc *RunContext) map[string]string {
	if rc.caller != nil {
		return rc.workflowCallSecrets
	}
	return rc.Config.Secrets
}

func getWorkflowVars(_ context.Context, rc *RunContext) map[string]string {
	return rc.Config.Vars
}
