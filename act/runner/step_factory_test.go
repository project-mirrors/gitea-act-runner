// Copyright 2026 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"testing"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
)

func TestStepFactoryNewStep(t *testing.T) {
	table := []struct {
		name  string
		model *model.Step
		check func(s step) bool
	}{
		{
			name: "StepRemoteAction",
			model: &model.Step{
				Uses: "remote/action@v1",
			},
			check: func(s step) bool {
				_, ok := s.(*stepActionRemote)
				return ok
			},
		},
		{
			name: "StepLocalAction",
			model: &model.Step{
				Uses: "./action@v1",
			},
			check: func(s step) bool {
				_, ok := s.(*stepActionLocal)
				return ok
			},
		},
		{
			name: "StepDocker",
			model: &model.Step{
				Uses: "docker://image:tag",
			},
			check: func(s step) bool {
				_, ok := s.(*stepDocker)
				return ok
			},
		},
		{
			name: "StepRun",
			model: &model.Step{
				Run: "cmd",
			},
			check: func(s step) bool {
				_, ok := s.(*stepRun)
				return ok
			},
		},
		{
			name: "StepBuiltinAction",
			model: &model.Step{
				Uses: "builtin:checkout",
			},
			check: func(s step) bool {
				_, ok := s.(*stepActionBuiltin)
				return ok
			},
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			sf := &stepFactoryImpl{}

			step, err := sf.newStep(tt.model, &RunContext{})

			assert.True(t, tt.check(step))
			assert.NoError(t, err)
		})
	}
}

func TestStepFactoryInvalidStep(t *testing.T) {
	for _, stepModel := range []*model.Step{
		{Uses: "remote/action@v1", Run: "cmd"},
		{Uses: "builtin:unknown"},
	} {
		_, err := (&stepFactoryImpl{}).newStep(stepModel, &RunContext{})
		assert.Error(t, err, stepModel.Uses)
	}
}
