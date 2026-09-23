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
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	_ "embed"

	"gitea.dev/actionslib/pkg/expreval"
	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"go.yaml.in/yaml/v4"
)

// NewExpressionEvaluator creates a new evaluator
func (rc *RunContext) NewExpressionEvaluator(ctx context.Context) *ExpressionEvaluator {
	return rc.NewExpressionEvaluatorWithEnv(ctx, rc.GetEnv())
}

func (rc *RunContext) NewExpressionEvaluatorWithEnv(ctx context.Context, env map[string]string) *ExpressionEvaluator {
	var workflowCallResult map[string]*model.WorkflowCallResult

	// todo: cleanup EvaluationEnvironment creation
	strategy := make(map[string]any)
	if rc.Run != nil {
		strategy = exprparser.StrategyContext(rc.Run.Job().Strategy, rc.jobIndex, rc.jobTotal)

		// only setup jobs context in case of workflow_call
		// and existing expression evaluator (this means, jobs are at
		// least ready to run)
		if rc.caller != nil && rc.ExprEval != nil {
			workflowCallResult = map[string]*model.WorkflowCallResult{}

			for jobName, job := range rc.Run.Workflow.Jobs {
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
		Needs:     exprparser.NeedsContext(rc.Run),
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
	return rc.newStepExpressionEvaluator(ctx, step, rc.actionInputs, rc.getJobContext())
}

// NewActionInputsExpressionEvaluator creates a new evaluator with the step's own with: values as `inputs`
func (rc *RunContext) NewActionInputsExpressionEvaluator(ctx context.Context, step step) *ExpressionEvaluator {
	return rc.newStepExpressionEvaluator(ctx, step, inputsFromEnv(*step.getEnv()), rc.getJobContext())
}

func (rc *RunContext) newStepExpressionEvaluator(ctx context.Context, step step, stepInputs map[string]any, jobContext *model.JobContext) *ExpressionEvaluator {
	// todo: cleanup EvaluationEnvironment creation
	ee := &exprparser.EvaluationEnvironment{
		Github:   step.getGithubContext(ctx),
		Env:      *step.getEnv(),
		Job:      jobContext,
		Steps:    rc.getStepsContext(),
		Secrets:  getWorkflowSecrets(rc),
		Vars:     getWorkflowVars(ctx, rc),
		Strategy: exprparser.StrategyContext(rc.Run.Job().Strategy, rc.jobIndex, rc.jobTotal),
		Matrix:   rc.Matrix,
		Needs:    exprparser.NeedsContext(rc.Run),
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

	switch ghc.EventName {
	case "workflow_dispatch":
		if config := rc.Run.Workflow.WorkflowDispatchConfig(); config != nil {
			for name, input := range config.Inputs {
				if input.Type == "number" {
					input.Type = "string" // boolean is the only typed dispatch input, as in Gitea
				}
				setEvaluatorInput(inputs, ghc, name, model.WorkflowCallInput{Type: input.Type, Default: input.Default})
			}
		}
	case "workflow_call":
		for name, input := range rc.Run.Workflow.WorkflowCallConfig().Inputs {
			setEvaluatorInput(inputs, ghc, name, input)
		}
	}
	return inputs
}

func setEvaluatorInput(inputs map[string]any, ghc *model.GithubContext, name string, input model.WorkflowCallInput) {
	value := model.NestedMapLookup(ghc.Event, "inputs", name)
	if value == nil {
		value, _ = input.DefaultValue()
	}
	inputs[name] = model.CoerceInputValue(value, input.Type)
}

// resolveWorkflowCall evaluates the caller's with: and secrets: once, as the server does before dispatching a called workflow.
func (rc *RunContext) resolveWorkflowCall(ctx context.Context) error {
	if rc.caller == nil {
		return nil
	}
	logger := common.Logger(ctx)
	callerJob := rc.caller.runContext.Run.Job()
	callerEval := rc.caller.runContext.ExprEval.shared(ctx)
	var callerWith map[string]any
	if err := model.DecodeEvaluated("workflow inputs", callerJob.RawWith, callerEval.EvaluateYamlNode, &callerWith); err != nil {
		return err
	}
	if !rc.Run.Workflow.IsWorkflowCall() {
		logger.Warn("workflow_call key is not defined in the referenced workflow")
	}
	config := rc.Run.Workflow.WorkflowCallConfig()
	var err error
	if rc.workflowCallInputs, err = callerEval.ResolveWorkflowCallInputs(config, callerWith); err != nil {
		logger.Warn(err)
	}

	if callerJob.InheritSecrets() {
		rc.workflowCallSecrets = getWorkflowSecrets(rc.caller.runContext)
		return nil
	}
	secrets := callerJob.Secrets()
	if err := config.ValidateSecrets(secrets); err != nil {
		logger.Warn(err)
	}
	if rc.workflowCallSecrets, err = callerEval.EvaluateCallerSecrets(secrets); err != nil {
		return fmt.Errorf("unable to interpolate %w", err)
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
