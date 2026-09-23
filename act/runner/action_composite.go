// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gitea.com/gitea/runner/act/common"

	"gitea.dev/actionslib/pkg/model"
)

func evaluateCompositeInputAndEnv(ctx context.Context, parent *RunContext, step actionStep) (map[string]string, error) {
	env := make(map[string]string)
	stepEnv := *step.getEnv()
	for k, v := range stepEnv {
		// do not set current inputs into composite action
		// the required inputs are added in the second loop
		if !strings.HasPrefix(k, "INPUT_") {
			env[k] = v
		}
	}

	ee := parent.NewActionInputsExpressionEvaluator(ctx, step)

	for inputID, input := range step.getActionModel().Inputs {
		envKey := regexp.MustCompile("[^A-Z0-9-]").ReplaceAllString(strings.ToUpper(inputID), "_")
		envKey = "INPUT_" + strings.ToUpper(envKey)

		// lookup if key is defined in the step but the already
		// evaluated value from the environment
		defined := false
		for key := range step.getStepModel().With {
			if strings.EqualFold(key, inputID) {
				defined = true
				break
			}
		}
		if value, ok := stepEnv[envKey]; defined && ok {
			env[envKey] = value
		} else {
			// defaults could contain expressions
			var err error
			if env[envKey], err = ee.Interpolate(ctx, input.Default); err != nil {
				return nil, fmt.Errorf("unable to interpolate the default of input %s: %w", inputID, err)
			}
		}
	}
	gh := step.getGithubContext(ctx)
	env["GITHUB_ACTION_REPOSITORY"] = gh.ActionRepository
	env["GITHUB_ACTION_REF"] = gh.ActionRef

	return env, nil
}

func (rc *RunContext) setCompositeActionEnv(env map[string]string) {
	rc.setActionEnv(env)
	for key := range rc.Env {
		if strings.HasPrefix(key, "INPUT_") {
			delete(rc.Env, key)
		}
	}
}

func newCompositeRunContext(ctx context.Context, parent *RunContext, step actionStep, actionPath string) (*RunContext, error) {
	env, err := evaluateCompositeInputAndEnv(ctx, parent, step)
	if err != nil {
		return nil, err
	}

	// run with the global config but without secrets
	configCopy := *parent.Config
	configCopy.Secrets = nil

	// create a run context for the composite action to run in
	compositerc := &RunContext{
		Name:    parent.Name,
		JobName: parent.JobName,
		Matrix:  parent.Matrix,
		Run: &model.Run{
			JobID: parent.Run.JobID,
			Workflow: &model.Workflow{
				Name: parent.Run.Workflow.Name,
				Jobs: map[string]*model.Job{
					parent.Run.JobID: {Strategy: parent.Run.Job().Strategy},
				},
			},
		},
		Config:        &configCopy,
		StepResults:   map[string]*model.StepResult{},
		JobContainer:  parent.JobContainer,
		ActionPath:    actionPath,
		GlobalEnv:     parent.GlobalEnv,
		Masks:         parent.Masks,
		ExtraPath:     parent.ExtraPath,
		Parent:        parent,
		EventJSON:     parent.EventJSON,
		platformImage: parent.platformImage,
		jobIndex:      parent.jobIndex,
		jobTotal:      parent.jobTotal,
	}
	compositerc.setCompositeActionEnv(env)
	compositerc.ExprEval = compositerc.NewExpressionEvaluator(ctx)

	return compositerc, nil
}

// appendUniqueMasks appends the masks from src to dst, skipping any mask that
// is already present in dst. This prevents the parent RunContext's Masks slice
// from growing exponentially when composite actions are nested or repeated,
// since each composite RunContext is seeded with its parent's masks.
func appendUniqueMasks(dst, src []string) []string {
	for _, m := range src {
		if !slices.Contains(dst, m) {
			dst = append(dst, m)
		}
	}
	return dst
}

func execAsComposite(step actionStep) common.Executor {
	rc := step.getRunContext()
	action := step.getActionModel()

	return func(ctx context.Context) error {
		compositeRC, err := step.getCompositeRunContext(ctx)
		if err != nil {
			return err
		}

		steps := step.getCompositeSteps()

		if steps == nil || steps.main == nil {
			return errors.New("missing steps in composite action")
		}

		ctx = WithCompositeLogger(ctx, &compositeRC.Masks)

		err = steps.main(ctx)

		// Map outputs from composite RunContext to job RunContext
		eval := compositeRC.NewExpressionEvaluator(ctx)
		for outputName, output := range action.Outputs {
			value, outputErr := eval.Interpolate(ctx, output.Value)
			if outputErr != nil {
				err = errors.Join(err, fmt.Errorf("unable to interpolate output %s: %w", outputName, outputErr))
				continue
			}
			rc.setOutput(ctx, map[string]string{"name": outputName}, value)
		}

		// compositeRC.Masks is seeded with rc.Masks (see newCompositeRunContext)
		// and may have additional masks appended while the composite action runs.
		// Only append masks that are not already present, otherwise nested or
		// repeated composite actions grow rc.Masks exponentially.
		rc.Masks = appendUniqueMasks(rc.Masks, compositeRC.Masks)
		rc.ExtraPath = compositeRC.ExtraPath
		// Propagate GlobalEnv only, so composite inputs and step-local values do not escape.
		mergeIntoMap := mergeIntoMapCaseSensitive
		if rc.JobContainer.IsEnvironmentCaseInsensitive() {
			mergeIntoMap = mergeIntoMapCaseInsensitive
		}
		if rc.GlobalEnv == nil {
			rc.GlobalEnv = map[string]string{}
		}
		mergeIntoMap(rc.GlobalEnv, compositeRC.GlobalEnv)
		mergeIntoMap(rc.Env, compositeRC.GlobalEnv)

		return err
	}
}

type compositeSteps struct {
	pre  common.Executor
	main common.Executor
	post common.Executor
}

// Executor returns a pipeline executor for all the steps in the job
func (rc *RunContext) compositeExecutor(action *model.Action) *compositeSteps {
	steps := make([]common.Executor, 0)
	preSteps := make([]common.Executor, 0)
	var postExecutor common.Executor

	sf := &stepFactoryImpl{}

	for i, step := range action.Runs.Steps {
		if step.ID == "" {
			step.ID = strconv.Itoa(i)
		}
		step.Number = i

		// create a copy of the step, since this composite action could
		// run multiple times and we might modify the instance
		stepcopy := step

		step, err := sf.newStep(&stepcopy, rc)
		if err != nil {
			return &compositeSteps{
				main: common.NewErrorExecutor(err),
			}
		}

		stepID := step.getStepModel().ID
		stepPre := rc.newCompositeCommandExecutor(step.pre())
		preSteps = append(preSteps, newCompositeStepLogExecutor(stepPre, stepID))

		steps = append(steps, newCompositeStepLogExecutor(rc.newCompositeCommandExecutor(step.main()), stepID))

		// run the post executor in reverse order
		if postExecutor != nil {
			stepPost := rc.newCompositeCommandExecutor(step.post())
			postExecutor = newCompositeStepLogExecutor(stepPost.Finally(postExecutor), stepID)
		} else {
			stepPost := rc.newCompositeCommandExecutor(step.post())
			postExecutor = newCompositeStepLogExecutor(stepPost, stepID)
		}
	}

	steps = append(steps, common.JobError)
	preSteps = append(preSteps, common.JobError)
	return &compositeSteps{
		pre: func(ctx context.Context) error {
			return common.NewPipelineExecutor(preSteps...)(common.WithJobErrorContainer(ctx))
		},
		main: func(ctx context.Context) error {
			return common.NewPipelineExecutor(steps...)(common.WithJobErrorContainer(ctx))
		},
		post: postExecutor,
	}
}

func (rc *RunContext) newCompositeCommandExecutor(executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		ctx = WithCompositeLogger(ctx, &rc.Masks)

		logWriter := rc.commandLogWriter(ctx)

		oldout, olderr := rc.JobContainer.ReplaceLogWriter(logWriter, logWriter)
		defer rc.JobContainer.ReplaceLogWriter(oldout, olderr)

		return executor(ctx)
	}
}

func newCompositeStepLogExecutor(runStep common.Executor, stepID string) common.Executor {
	return func(ctx context.Context) error {
		ctx = WithCompositeStepLogger(ctx, stepID)
		logger := common.Logger(ctx)
		err := runStep(ctx)
		if err != nil {
			logger.Errorf("##[error]%s", EscapeCommandData(err.Error()))
			common.SetJobError(ctx, err)
		} else if ctx.Err() != nil {
			logger.Errorf("##[error]%s", EscapeCommandData(ctx.Err().Error()))
			common.SetJobError(ctx, ctx.Err())
		}
		return nil
	}
}
