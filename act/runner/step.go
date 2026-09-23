// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"fmt"
	maps0 "maps"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"go.yaml.in/yaml/v4"
)

type step interface {
	pre() common.Executor
	main() common.Executor
	post() common.Executor

	getRunContext() *RunContext
	getGithubContext(ctx context.Context) *model.GithubContext
	getStepModel() *model.Step
	getEnv() *map[string]string
	getIfExpression(context context.Context, stage stepStage) string
}

type stepStage int

const (
	stepStagePre stepStage = iota
	stepStageMain
	stepStagePost
)

// Controls how many symlinks are resolved for local and remote Actions
const maxSymlinkDepth = 10

func (s stepStage) String() string {
	switch s {
	case stepStagePre:
		return "Pre"
	case stepStageMain:
		return "Main"
	case stepStagePost:
		return "Post"
	}
	return "Unknown"
}

func processRunnerEnvFileCommand(ctx context.Context, fileName string, rc *RunContext, setter func(context.Context, map[string]string, string)) error {
	env := map[string]string{}
	err := rc.JobContainer.UpdateFromEnv(path.Join(rc.JobContainer.GetActPath(), fileName), &env)(ctx)
	for k, v := range env {
		setter(ctx, map[string]string{"name": k}, v)
	}
	return err
}

func runStepExecutor(step step, stage stepStage, executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		rc := step.getRunContext()
		stepModel := step.getStepModel()

		ifExpression := step.getIfExpression(ctx, stage)
		rc.CurrentStep = stepModel.ID

		stepResult := &model.StepResult{
			Outcome:    model.StepStatusSuccess,
			Conclusion: model.StepStatusSuccess,
			Outputs:    make(map[string]string),
		}
		if stage == stepStageMain {
			rc.StepResults[rc.CurrentStep] = stepResult
		}

		err := setupEnv(ctx, step)
		var runStep bool
		if err == nil {
			runStep, err = isStepEnabled(ctx, ifExpression, step, stage)
		}
		if err != nil {
			stepResult.Conclusion = model.StepStatusFailure
			stepResult.Outcome = model.StepStatusFailure
			logger.WithField("stepResult", stepResult.Conclusion).Infof("Failure - %s %s", stage, stepModel)
			return err
		}

		if !runStep {
			stepResult.Conclusion = model.StepStatusSkipped
			stepResult.Outcome = model.StepStatusSkipped
			logger.WithField("stepResult", stepResult.Conclusion).Debugf("Skipping step '%s' due to '%s'", stepModel, ifExpression)
			return nil
		}

		stepString := rc.ExprEval.InterpolateName(ctx, stepModel.String())
		if strings.Contains(stepString, "::add-mask::") {
			stepString = "add-mask command"
		}
		if stage == stepStageMain {
			// Main steps print their own raw "Run <title>" header, so this line is redundant and
			// only leaks into the "Set up job" section for the first step; keep it as a debug trace.
			logger.Debugf("Run %s %s", stage, stepString)
		} else {
			logger.Infof("Run %s %s", stage, stepString)
		}

		// Prepare and clean Runner File Commands
		actPath := rc.JobContainer.GetActPath()

		outputFileCommand := path.Join("workflow", "outputcmd.txt")
		(*step.getEnv())["GITHUB_OUTPUT"] = path.Join(actPath, outputFileCommand)

		stateFileCommand := path.Join("workflow", "statecmd.txt")
		(*step.getEnv())["GITHUB_STATE"] = path.Join(actPath, stateFileCommand)

		pathFileCommand := path.Join("workflow", "pathcmd.txt")
		(*step.getEnv())["GITHUB_PATH"] = path.Join(actPath, pathFileCommand)

		envFileCommand := path.Join("workflow", "envs.txt")
		(*step.getEnv())["GITHUB_ENV"] = path.Join(actPath, envFileCommand)

		// Per-step summary file. Composite sub-steps share the outer job step's index
		// via the Parent chain so all writes from within a composite action accumulate
		// in the same file and upload under the outer step_index.
		topRC := rc.topLevelRunContext()
		stepSummaryIndex := topRC.CurrentStepIndex
		summaryFileCommand := path.Join("workflow", "step-summary-"+strconv.Itoa(stepSummaryIndex)+".md")
		(*step.getEnv())["GITHUB_STEP_SUMMARY"] = path.Join(actPath, summaryFileCommand)

		{
			// For Gitea
			(*step.getEnv())["GITEA_OUTPUT"] = (*step.getEnv())["GITHUB_OUTPUT"]
			(*step.getEnv())["GITEA_STATE"] = (*step.getEnv())["GITHUB_STATE"]
			(*step.getEnv())["GITEA_PATH"] = (*step.getEnv())["GITHUB_PATH"]
			(*step.getEnv())["GITEA_ENV"] = (*step.getEnv())["GITHUB_ENV"]
			(*step.getEnv())["GITEA_STEP_SUMMARY"] = (*step.getEnv())["GITHUB_STEP_SUMMARY"]
		}

		// Reset the per-phase file-command files. GITHUB_STEP_SUMMARY is intentionally
		// excluded here and initialized below at most once per step so writes from later
		// phases and from composite sub-steps accumulate instead of being truncated.
		files := []*container.FileEntry{
			{Name: outputFileCommand, Mode: 0o666},
			{Name: stateFileCommand, Mode: 0o666},
			{Name: pathFileCommand, Mode: 0o666},
			{Name: envFileCommand, Mode: 0o666},
		}
		if topRC.summaryFileInitialized == nil {
			topRC.summaryFileInitialized = map[int]bool{}
		}
		_ = rc.JobContainer.Copy(actPath, files...)(ctx)
		if !topRC.summaryFileInitialized[stepSummaryIndex] {
			_ = rc.JobContainer.Copy(actPath, &container.FileEntry{Name: summaryFileCommand, Mode: 0o666})(ctx)
			topRC.summaryFileInitialized[stepSummaryIndex] = true
		}

		// The command handler needs the step's env to judge ACTIONS_ALLOW_UNSECURE_COMMANDS.
		// Cloned: the step executor keeps writing to its own env map after this point, on a
		// different goroutine from the command handler that reads it.
		rc.setCurrentStepEnv(maps0.Clone(*step.getEnv()))
		defer rc.setCurrentStepEnv(nil)
		_ = rc.takeUnsecureCommandError() // a refusal from before any step belongs to no step

		timeoutctx, cancelTimeOut := evaluateStepTimeout(ctx, rc.ExprEval, stepModel)
		defer cancelTimeOut()
		err = executor(timeoutctx)
		// Always take it, so the job-scoped error cannot leak onto a later step. A refusal
		// fails the step as it does on GitHub, but the executor's own error wins.
		insecureErr := rc.takeUnsecureCommandError()
		if err == nil {
			err = insecureErr
		}
		if fileErr := processRunnerEnvFileCommand(ctx, envFileCommand, rc, rc.setEnvFile); fileErr != nil && err == nil {
			err = fileErr
		}
		if fileErr := processRunnerEnvFileCommand(ctx, stateFileCommand, rc, rc.saveState); fileErr != nil && err == nil {
			err = fileErr
		}
		if fileErr := processRunnerEnvFileCommand(ctx, outputFileCommand, rc, rc.setOutput); fileErr != nil && err == nil {
			err = fileErr
		}
		if fileErr := rc.UpdateExtraPath(ctx, path.Join(actPath, pathFileCommand)); fileErr != nil && err == nil {
			err = fileErr
		}
		_ = rc.JobContainer.Copy(actPath, files...)(ctx)

		if err == nil {
			logger.WithField("stepResult", stepResult.Conclusion).Infof("Success - %s %s", stage, stepString)
		} else {
			stepResult.Outcome = model.StepStatusFailure

			continueOnError, parseErr := isContinueOnError(ctx, stepModel.RawContinueOnError, step, stage)
			if parseErr != nil {
				stepResult.Conclusion = model.StepStatusFailure
				logger.WithField("stepResult", stepResult.Conclusion).Infof("Failure - %s %s", stage, stepString)
				return parseErr
			}

			if continueOnError {
				logger.Errorf("##[error]%s", EscapeCommandData(err.Error()))
				logger.Infof("Failed but continue next step")
				err = nil
				stepResult.Conclusion = model.StepStatusSuccess
			} else {
				stepResult.Conclusion = model.StepStatusFailure
			}

			// Infof: Errorf entries are promoted to the user log by the reporter,
			// which would duplicate the ##[error] annotation emitted elsewhere.
			logger.WithField("stepResult", stepResult.Conclusion).Infof("Failure - %s %s", stage, stepString)
		}
		return err
	}
}

func evaluateStepTimeout(ctx context.Context, exprEval *expressionEvaluator, stepModel *model.Step) (context.Context, context.CancelFunc) {
	timeout, err := exprEval.Interpolate(ctx, stepModel.TimeoutMinutes)
	if err != nil {
		common.Logger(ctx).Errorf("An error occurred when attempting to determine the step timeout: %s", err)
	} else if timeout != "" {
		if timeOutMinutes, err := strconv.ParseInt(timeout, 10, 64); err == nil && timeOutMinutes > 0 {
			return context.WithTimeout(ctx, time.Duration(timeOutMinutes)*time.Minute)
		}
	}
	return ctx, func() {}
}

func setupEnv(ctx context.Context, step step) error {
	rc := step.getRunContext()

	mergeEnv(ctx, step)
	// merge step env last, since it should not be overwritten
	mergeIntoMap(step, step.getEnv(), step.getStepModel().GetEnv())

	var err error
	exprEval := rc.NewExpressionEvaluator(ctx)
	for k, v := range *step.getEnv() {
		if !strings.HasPrefix(k, "INPUT_") {
			if (*step.getEnv())[k], err = exprEval.Interpolate(ctx, v); err != nil {
				return fmt.Errorf("unable to interpolate env %s: %w", k, err)
			}
		}
	}
	// after we have an evaluated step context, update the expressions evaluator with a new env context
	// you can use step level env in the with property of a uses construct
	inputEval := sync.OnceValue(func() *expressionEvaluator { return rc.NewExpressionEvaluatorWithEnv(ctx, *step.getEnv()) })
	for k, v := range *step.getEnv() {
		if strings.HasPrefix(k, "INPUT_") {
			if (*step.getEnv())[k], err = inputEval().Interpolate(ctx, v); err != nil {
				return fmt.Errorf("unable to interpolate env %s: %w", k, err)
			}
		}
	}
	if step.getStepModel().RawWith.Kind == yaml.ScalarNode {
		decoded := &model.Step{}
		if err := decodeDeferred(ctx, inputEval(), "with", step.getStepModel().RawWith, &decoded.With); err != nil {
			return err
		}
		step.getStepModel().With = decoded.With
		mergeIntoMap(step, step.getEnv(), decoded.GetEnv())
	}
	return nil
}

func mergeEnv(ctx context.Context, step step) {
	env := step.getEnv()
	rc := step.getRunContext()
	job := rc.Run.Job()

	c := job.Container()
	if c != nil {
		// container env is the image's baseline, which job env and $GITHUB_ENV override
		mergeIntoMap(step, env, c.Env, rc.GetEnv())
	} else {
		mergeIntoMap(step, env, rc.GetEnv())
	}

	rc.withGithubEnv(ctx, step.getGithubContext(ctx), *env)
}

func isStepEnabled(ctx context.Context, expr string, step step, stage stepStage) (bool, error) {
	rc := step.getRunContext()

	var defaultStatusCheck exprparser.DefaultStatusCheck
	if stage == stepStagePost {
		defaultStatusCheck = exprparser.DefaultStatusCheckAlways
	} else {
		defaultStatusCheck = exprparser.DefaultStatusCheckSuccess
	}

	// success() and failure() in a composite's own main steps read the composite's result, other stages the job's
	jobContext := rc.getJobContext()
	if rc.Parent != nil && stage == stepStageMain && jobContext.Status != "cancelled" {
		jobContext.Status = rc.ownStatus()
	}
	runStep, err := EvalBool(ctx, rc.newStepExpressionEvaluator(ctx, step, rc.actionInputs, jobContext), expr, defaultStatusCheck)
	if err != nil {
		return false, fmt.Errorf("if-expression %q evaluation failed: %s", expr, err)
	}

	return runStep, nil
}

func isContinueOnError(ctx context.Context, expr string, step step, _ stepStage) (bool, error) {
	// https://github.com/github/docs/blob/3ae84420bd10997bb5f35f629ebb7160fe776eae/content/actions/reference/workflow-syntax-for-github-actions.md?plain=true#L962
	if len(strings.TrimSpace(expr)) == 0 {
		return false, nil
	}

	rc := step.getRunContext()

	continueOnError, err := EvalBool(ctx, rc.NewStepExpressionEvaluator(ctx, step), expr, exprparser.DefaultStatusCheckNone)
	if err != nil {
		return false, fmt.Errorf("continue-on-error expression %q evaluation failed: %s", expr, err)
	}

	return continueOnError, nil
}

func mergeIntoMap(step step, target *map[string]string, maps ...map[string]string) {
	if rc := step.getRunContext(); rc != nil && rc.JobContainer != nil && rc.JobContainer.IsEnvironmentCaseInsensitive() {
		mergeIntoMapCaseInsensitive(*target, maps...)
	} else {
		mergeIntoMapCaseSensitive(*target, maps...)
	}
}

func mergeIntoMapCaseSensitive(target map[string]string, maps ...map[string]string) {
	for _, m := range maps {
		maps0.Copy(target, m)
	}
}

func mergeIntoMapCaseInsensitive(target map[string]string, maps ...map[string]string) {
	foldKeys := make(map[string]string, len(target))
	for k := range target {
		foldKeys[strings.ToLower(k)] = k
	}
	toKey := func(s string) string {
		foldKey := strings.ToLower(s)
		if k, ok := foldKeys[foldKey]; ok {
			return k
		}
		foldKeys[strings.ToLower(foldKey)] = s
		return s
	}
	for _, m := range maps {
		for k, v := range m {
			target[toKey(k)] = v
		}
	}
}

func symlinkJoin(filename, sym, parent string) (string, error) {
	dir := path.Dir(filename)
	dest := path.Join(dir, sym)
	prefix := path.Clean(parent) + "/"
	if strings.HasPrefix(dest, prefix) || prefix == "./" {
		return dest, nil
	}
	return "", fmt.Errorf("symlink tries to access file '%s' outside of '%s'", strings.ReplaceAll(dest, "'", "''"), strings.ReplaceAll(parent, "'", "''"))
}
