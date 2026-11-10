// Copyright 2026 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"testing"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStepFactoryNewStep(t *testing.T) {
	for _, test := range []struct {
		name  string
		model *model.Step
		want  step
	}{
		{"StepRemoteAction", &model.Step{Uses: "remote/action@v1"}, &stepActionRemote{}},
		{"StepLocalAction", &model.Step{Uses: "./action@v1"}, &stepActionLocal{}},
		{"StepDocker", &model.Step{Uses: "docker://image:tag"}, &stepDocker{}},
		{"StepRun", &model.Step{Run: "cmd"}, &stepRun{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			step, err := (&stepFactoryImpl{}).newStep(test.model, &RunContext{})
			require.NoError(t, err)
			assert.IsType(t, test.want, step)
		})
	}
}

func TestStepFactoryInvalidStep(t *testing.T) {
	for _, stepModel := range []*model.Step{
		{Uses: "remote/action@v1", Run: "cmd"},
	} {
		_, err := (&stepFactoryImpl{}).newStep(stepModel, &RunContext{Name: "build"})
		assert.ErrorContains(t, err, "job:build ", stepModel.Uses)
	}
}
