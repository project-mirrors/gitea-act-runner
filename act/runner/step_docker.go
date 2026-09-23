// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/kballard/go-shellquote"
)

type stepDocker struct {
	Step       *model.Step
	RunContext *RunContext
	env        map[string]string
}

func (sd *stepDocker) pre() common.Executor { return common.NewPipelineExecutor() }

func (sd *stepDocker) main() common.Executor {
	sd.env = map[string]string{}

	return runStepExecutor(sd, stepStageMain, sd.runUsesContainer())
}

func (sd *stepDocker) post() common.Executor { return common.NewPipelineExecutor() }

func (sd *stepDocker) getRunContext() *RunContext {
	return sd.RunContext
}

func (sd *stepDocker) getGithubContext(ctx context.Context) *model.GithubContext {
	return sd.getRunContext().getGithubContext(ctx)
}

func (sd *stepDocker) getStepModel() *model.Step {
	return sd.Step
}

func (sd *stepDocker) getEnv() *map[string]string {
	return &sd.env
}

func (sd *stepDocker) getIfExpression(_ context.Context, _ stepStage) string {
	return sd.Step.If.Value
}

func (sd *stepDocker) runUsesContainer() common.Executor {
	rc := sd.RunContext
	step := sd.Step

	return func(ctx context.Context) error {
		image := strings.TrimPrefix(step.Uses, "docker://")
		cmd, err := shellquote.Split(step.With["args"])
		if err != nil {
			return err
		}

		var entrypoint []string
		if entry := step.With["entrypoint"]; entry != "" {
			entrypoint = []string{entry}
		}

		stepContainer := newStepContainer(ctx, sd, image, cmd, entrypoint, "")

		return common.NewPipelineExecutor(
			stepContainer.Pull(rc.Config.ForcePull),
			stepContainer.Remove(),
			stepContainer.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
			stepContainer.Start(true),
		).Finally(stepContainer.Close())(ctx)
	}
}

var ContainerNewContainer = container.NewContainer
