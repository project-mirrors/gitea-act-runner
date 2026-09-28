// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"gitea.dev/actionslib/pkg/model"
)

type stepFactory interface {
	newStep(step *model.Step, rc *RunContext) (step, error)
}

type stepFactoryImpl struct{}

func (sf *stepFactoryImpl) newStep(stepModel *model.Step, rc *RunContext) (step, error) {
	switch stepModel.Type() {
	case model.StepTypeInvalid:
		return nil, fmt.Errorf("invalid run/uses syntax for job:%s step:%+v", rc.Run, stepModel)
	case model.StepTypeRun:
		return &stepRun{
			Step:       stepModel,
			RunContext: rc,
		}, nil
	case model.StepTypeUsesActionLocal:
		return &stepActionLocal{
			Step:       stepModel,
			RunContext: rc,
			readAction: readActionImpl,
			runAction:  runActionImpl,
		}, nil
	case model.StepTypeUsesActionRemote:
		if name, ok := strings.CutPrefix(stepModel.Uses, "builtin:"); ok {
			run, ok := builtinActions[name]
			if !ok {
				return nil, fmt.Errorf("unknown built-in action %q (known: %v) for job:%s step:%+v",
					name, slices.Sorted(maps.Keys(builtinActions)), rc.Run, stepModel)
			}
			return &stepActionBuiltin{
				Step:       stepModel,
				RunContext: rc,
				run:        run,
			}, nil
		}
		return &stepActionRemote{
			Step:       stepModel,
			RunContext: rc,
			readAction: readActionImpl,
			runAction:  runActionImpl,
		}, nil
	case model.StepTypeUsesDockerURL:
		return &stepDocker{
			Step:       stepModel,
			RunContext: rc,
		}, nil
	}

	return nil, fmt.Errorf("unable to determine how to run job:%s step:%+v", rc.Run, stepModel)
}
