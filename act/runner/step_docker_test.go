// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestStepDockerMain(t *testing.T) {
	cm := &containerMock{}

	var input *container.NewContainerInput

	// mock the new container call
	origContainerNewContainer := ContainerNewContainer
	ContainerNewContainer = func(containerInput *container.NewContainerInput) container.ExecutionsEnvironment {
		input = containerInput
		return cm
	}
	defer func() {
		ContainerNewContainer = origContainerNewContainer
	}()

	ctx := context.Background()

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

	cm.On("Copy", "/var/run/act", mock.AnythingOfType("[]*container.FileEntry")).Return(noopExecutor)

	cm.On("UpdateFromEnv", "/var/run/act/workflow/envs.txt", mock.AnythingOfType("*map[string]string")).Return(noopExecutor)

	cm.On("UpdateFromEnv", "/var/run/act/workflow/statecmd.txt", mock.AnythingOfType("*map[string]string")).Return(noopExecutor)

	cm.On("UpdateFromEnv", "/var/run/act/workflow/outputcmd.txt", mock.AnythingOfType("*map[string]string")).Return(noopExecutor)

	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/pathcmd.txt").Return(io.NopCloser(&bytes.Buffer{}), nil)

	err := sd.main()(ctx)
	assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act

	assert.Equal(t, "node:14", input.Image)

	// DOCKER_USERNAME/DOCKER_PASSWORD secrets should not be used as implicit pull credentials for docker:// action containers.
	assert.Empty(t, input.Username)
	assert.Empty(t, input.Password)
	assert.True(t, input.AutoRemove)

	cm.AssertExpectations(t)
}

func TestStepDockerNewStepContainerAllocatePTY(t *testing.T) {
	for _, tc := range []struct {
		name     string
		allocPTY bool
	}{
		{name: "off", allocPTY: false},
		{name: "on", allocPTY: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			ctx := context.Background()
			sd := &stepDocker{
				RunContext: &RunContext{
					StepResults: map[string]*model.StepResult{},
					Config: &Config{
						AllocatePTY: tc.allocPTY,
						PlatformPicker: func(_ []string) string {
							return "node:14"
						},
					},
					Run: &model.Run{
						JobID: "1",
						Workflow: &model.Workflow{
							Jobs: map[string]*model.Job{"1": {}},
						},
					},
					JobContainer: cm,
				},
				Step: &model.Step{ID: "1", Uses: "docker://node:14"},
			}
			sd.RunContext.ExprEval = sd.RunContext.NewExpressionEvaluator(ctx)

			newStepContainer(ctx, sd, "node:14", []string{"echo", "hi"}, nil, "")
			assert.Equal(t, tc.allocPTY, captured.AllocatePTY)
		})
	}
}

func TestStepDockerNewStepContainerNetworkModeAndPath(t *testing.T) {
	inheritedPath := (&container.LinuxContainerEnvironmentExtensions{}).DefaultPathVariable()
	cases := []struct {
		name          string
		platform      string
		path          string
		expectDefault bool
		keepsPath     bool
	}{
		{
			name:          "docker mode attaches to job container network",
			platform:      "node:14",
			path:          inheritedPath,
			expectDefault: false,
			keepsPath:     true,
		},
		{
			name:          "host mode uses default network and the image's PATH",
			platform:      "-self-hosted",
			path:          inheritedPath,
			expectDefault: true,
		},
		{
			name:          "host mode keeps a PATH that is not the runner's",
			platform:      "-self-hosted",
			path:          "/custom/bin",
			expectDefault: true,
			keepsPath:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			ctx := context.Background()

			platform := tc.platform
			sd := &stepDocker{
				RunContext: &RunContext{
					StepResults: map[string]*model.StepResult{},
					Config: &Config{
						PlatformPicker: func(_ []string) string {
							return platform
						},
					},
					Run: &model.Run{
						JobID: "1",
						Workflow: &model.Workflow{
							Jobs: map[string]*model.Job{
								"1": {},
							},
						},
					},
					JobContainer: cm,
				},
				Step: &model.Step{
					ID:   "1",
					Uses: "docker://alpine:3.20",
				},
				env: map[string]string{"PATH": tc.path},
			}
			sd.RunContext.ExprEval = sd.RunContext.NewExpressionEvaluator(ctx)

			require.NoError(t, sd.RunContext.resolvePlatformImage(ctx))
			assert.Equal(t, tc.expectDefault, sd.RunContext.IsHostEnv(), "IsHostEnv mismatch for platform %q", tc.platform)

			newStepContainer(ctx, sd, "alpine:3.20", []string{"echo", "hello"}, nil, "")
			assert.Equal(t, tc.keepsPath, slices.Contains(captured.Env, "PATH="+tc.path))

			if tc.expectDefault {
				assert.Equal(t, "default", captured.NetworkMode,
					"host-mode step container must use 'default' network, got %q",
					captured.NetworkMode)
			} else {
				assert.True(t, strings.HasPrefix(captured.NetworkMode, "container:"),
					"docker-mode step container must attach to job container network, got %q",
					captured.NetworkMode)
			}
		})
	}
}
