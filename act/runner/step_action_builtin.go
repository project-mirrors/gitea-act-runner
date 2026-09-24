// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/checkout"

	"gitea.dev/actionslib/pkg/model"
)

var builtinActions = map[string]func(context.Context, *action.Context) error{
	"checkout": checkout.Main,
}

type stepActionBuiltin struct {
	Step       *model.Step
	RunContext *RunContext
	run        func(context.Context, *action.Context) error
	env        map[string]string
}

func (sab *stepActionBuiltin) pre() common.Executor { return common.NewPipelineExecutor() }

func (sab *stepActionBuiltin) main() common.Executor {
	sab.env = map[string]string{}
	return runStepExecutor(sab, stepStageMain, func(ctx context.Context) error {
		if common.Dryrun(ctx) {
			return nil
		}

		printRunActionHeader(ctx, sab.Step, sab.env, sab.RunContext)
		rawLogger := common.Logger(ctx).WithField(rawOutputField, true)
		defer rawLogger.Infof("::endgroup::")

		rc := sab.RunContext
		return sab.run(ctx, &action.Context{
			Container:       rc.JobContainer,
			Github:          rc.getGithubContext(ctx),
			Inputs:          sab.Step.With,
			HostWorkdir:     rc.Config.Workdir,
			Workspace:       rc.JobContainer.ToContainerPath(rc.Config.Workdir),
			BindWorkdir:     rc.Config.BindWorkdir,
			NoSkipCheckout:  rc.Config.NoSkipCheckout,
			UseGitIgnore:    rc.Config.UseGitIgnore,
			InsecureSkipTLS: rc.Config.InsecureSkipTLS,
		})
	})
}

func (sab *stepActionBuiltin) post() common.Executor { return common.NewPipelineExecutor() }

func (sab *stepActionBuiltin) getRunContext() *RunContext { return sab.RunContext }

func (sab *stepActionBuiltin) getGithubContext(ctx context.Context) *model.GithubContext {
	return sab.RunContext.getGithubContext(ctx)
}

func (sab *stepActionBuiltin) getStepModel() *model.Step { return sab.Step }

func (sab *stepActionBuiltin) getEnv() *map[string]string { return &sab.env }

func (sab *stepActionBuiltin) getIfExpression(_ context.Context, _ stepStage) string {
	return sab.Step.If.Value
}
