// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"slices"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStepDockerMain(t *testing.T) {
	cm := &containerMock{}

	var input *container.NewContainerInput

	origContainerNewContainer := ContainerNewContainer
	ContainerNewContainer = func(containerInput *container.NewContainerInput) container.ExecutionsEnvironment {
		input = containerInput
		return cm
	}
	defer func() {
		ContainerNewContainer = origContainerNewContainer
	}()

	ctx := t.Context()

	sd := &stepDocker{
		RunContext: &RunContext{
			StepResults: map[string]*model.StepResult{},
			Config: &Config{
				Secrets: map[string]string{
					"DOCKER_USERNAME": "docker-user",
					"DOCKER_PASSWORD": "docker-password",
				},
			},
			Run: &model.Run{
				JobID: "1",
				Workflow: &model.Workflow{
					Jobs: map[string]*model.Job{
						"1": {
							Defaults: model.Defaults{
								Run: model.RunDefaults{
									Shell: "bash",
								},
							},
						},
					},
				},
			},
			JobContainer: cm,
		},
		Step: &model.Step{
			ID:               "1",
			Uses:             "docker://node:14",
			WorkingDirectory: "workdir",
		},
	}
	sd.RunContext.ExprEval = sd.RunContext.NewExpressionEvaluator(ctx)

	cm.On("Pull", false).Return(noopExecutor)

	cm.On("Remove").Return(noopExecutor)

	cm.On("Create", []string(nil), []string(nil)).Return(noopExecutor)

	cm.On("Start", true).Return(noopExecutor)

	cm.On("Close").Return(noopExecutor)

	expectStepFileCommands(t, cm)
	require.NoError(t, sd.main()(ctx))

	assert.Equal(t, "node:14", input.Image)

	assert.Empty(t, input.Username)
	assert.Empty(t, input.Password)
	assert.True(t, input.AutoRemove)

	cm.AssertExpectations(t)
}

func TestStepDockerNewStepContainer(t *testing.T) {
	inheritedPath := (&container.LinuxContainerEnvironmentExtensions{}).DefaultPathVariable()
	for _, test := range []struct {
		name, platform, image, path, greeting string
		allocPTY, expectDefault, keepsPath    bool
	}{
		{name: "allocate PTY/off", platform: "node:14", image: "node:14", greeting: "hi"},
		{name: "allocate PTY/on", platform: "node:14", image: "node:14", greeting: "hi", allocPTY: true},
		{name: "docker mode attaches to job container network", platform: "node:14", image: "alpine:3.20", path: inheritedPath, greeting: "hello", keepsPath: true},
		{name: "host mode uses default network and the image's PATH", platform: "-self-hosted", image: "alpine:3.20", path: inheritedPath, greeting: "hello", expectDefault: true},
		{name: "host mode keeps a PATH that is not the runner's", platform: "-self-hosted", image: "alpine:3.20", path: "/custom/bin", greeting: "hello", expectDefault: true, keepsPath: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cm := &containerMock{}
			var captured *container.NewContainerInput
			origContainerNewContainer := ContainerNewContainer
			ContainerNewContainer = func(input *container.NewContainerInput) container.ExecutionsEnvironment {
				captured = input
				return cm
			}
			defer func() {
				ContainerNewContainer = origContainerNewContainer
			}()

			ctx := t.Context()
			sd := &stepDocker{
				RunContext: &RunContext{
					StepResults: map[string]*model.StepResult{},
					Config: &Config{
						AllocatePTY: test.allocPTY,
						PlatformPicker: func(_platforms []string) string {
							return test.platform
						},
					},
					Run:          &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
					JobContainer: cm,
				},
				Step: &model.Step{ID: "1", Uses: "docker://" + test.image},
			}
			sd.RunContext.ExprEval = sd.RunContext.NewExpressionEvaluator(ctx)
			if test.path != "" {
				sd.env = map[string]string{"PATH": test.path}
				require.NoError(t, sd.RunContext.resolvePlatformImage(ctx))
				assert.Equal(t, test.expectDefault, sd.RunContext.IsHostEnv(), "IsHostEnv mismatch for platform %q", test.platform)
			}

			newStepContainer(ctx, sd, test.image, []string{"echo", test.greeting}, nil, "")
			assert.Equal(t, test.allocPTY, captured.AllocatePTY)
			if test.path != "" {
				assert.Equal(t, test.keepsPath, slices.Contains(captured.Env, "PATH="+test.path))
				if test.expectDefault {
					assert.Equal(t, "default", captured.NetworkMode,
						"host-mode step container must use 'default' network, got %q", captured.NetworkMode)
				} else {
					assert.True(t, strings.HasPrefix(captured.NetworkMode, "container:"),
						"docker-mode step container must attach to job container network, got %q", captured.NetworkMode)
				}
			}
		})
	}
}
