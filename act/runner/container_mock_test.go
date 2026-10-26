// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"io"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	"github.com/stretchr/testify/mock"
)

var noopExecutor = func(context.Context) error { return nil }

type linuxEnvironment struct {
	container.LinuxContainerEnvironmentExtensions
}

func (linuxEnvironment) GetRunnerContext(context.Context) map[string]any {
	return map[string]any{"os": "Linux", "arch": "X64", "temp": "/tmp", "tool_cache": container.DefaultToolCache}
}

type containerMock struct {
	mock.Mock
	container.Container
	linuxEnvironment
}

func (cm *containerMock) Create(capAdd, capDrop []string) common.Executor {
	return cm.Called(capAdd, capDrop).Get(0).(func(context.Context) error)
}

func (cm *containerMock) Pull(forcePull bool) common.Executor {
	return cm.Called(forcePull).Get(0).(func(context.Context) error)
}

func (cm *containerMock) Start(attach bool) common.Executor {
	return cm.Called(attach).Get(0).(func(context.Context) error)
}

func (cm *containerMock) Remove() common.Executor {
	return cm.Called().Get(0).(func(context.Context) error)
}

func (cm *containerMock) Close() common.Executor {
	return cm.Called().Get(0).(func(context.Context) error)
}

func (cm *containerMock) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return cm.Called(srcPath, env).Get(0).(func(context.Context) error)
}

func (cm *containerMock) Copy(destPath string, files ...*container.FileEntry) common.Executor {
	return cm.Called(destPath, files).Get(0).(func(context.Context) error)
}

func (cm *containerMock) CopyDir(destPath, srcPath string, useGitIgnore, skipGitDir bool) common.Executor {
	return cm.Called(destPath, srcPath, useGitIgnore, skipGitDir).Get(0).(func(context.Context) error)
}

func (cm *containerMock) Exec(command []string, env map[string]string, user, workdir string) common.Executor {
	return cm.Called(command, env, user, workdir).Get(0).(func(context.Context) error)
}

func (cm *containerMock) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	args := cm.Called(ctx, srcPath)
	err, _ := args.Get(1).(error)
	return args.Get(0).(io.ReadCloser), err
}

func (cm *containerMock) DumpLogs(ctx context.Context) error {
	return cm.Called(ctx).Error(0)
}

func (cm *containerMock) Inspect(ctx context.Context) (*container.Info, error) {
	args := cm.Called(ctx)
	info, _ := args.Get(0).(*container.Info)
	err, _ := args.Get(1).(error)
	return info, err
}
