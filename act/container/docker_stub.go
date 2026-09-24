// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2023 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build WITHOUT_DOCKER || !(linux || darwin || windows || netbsd)

package container

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
	"time"

	"gitea.com/gitea/runner/act/common"

	"github.com/moby/moby/api/types/system"
)

func IsolatedCacheContainer(context.Context, netip.Addr, string) (string, error) {
	return "", nil
}

// ImageExistsLocally returns a boolean indicating if an image with the
// requested name, tag and architecture exists in the local docker image store
func ImageExistsLocally(ctx context.Context, imageName, platform string) (bool, error) {
	return false, errors.New("Unsupported Operation")
}

// RemoveImage removes image from local store, the function is used to run different
// container image architectures
func RemoveImage(ctx context.Context, imageName string, force, pruneChildren bool) (bool, error) {
	return false, errors.New("Unsupported Operation")
}

func RemoveDockerJobResources(_ context.Context, _ string) error {
	return nil
}

func NewDockerProxy(_ context.Context, _ string) *DockerProxy {
	return nil
}

func StartDockerProxy(daemonSocket, dir, job string) (*DockerProxy, error) {
	return nil, errors.New("Unsupported Operation")
}

// NewDockerBuildExecutor function to create a run executor for the container
func NewDockerBuildExecutor(input NewDockerBuildExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return errors.New("Unsupported Operation")
	}
}

// NewDockerPullExecutor function to create a run executor for the container
func NewDockerPullExecutor(input NewDockerPullExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return errors.New("Unsupported Operation")
	}
}

// NewContainer creates a reference to a container
func NewContainer(input *NewContainerInput) ExecutionsEnvironment {
	return nil
}

func RunnerArch(ctx context.Context) string {
	return runtime.GOOS
}

func GetHostInfo(ctx context.Context) (info system.Info, err error) {
	return system.Info{}, nil
}

func NewDockerVolumeRemoveExecutor(volume string, force bool) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func NewDockerNetworkCreateExecutor(name string, opts NewDockerNetworkCreateExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func NewDockerNetworkRemoveExecutor(name string) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}

func NewDockerNetworkConnectExecutor(_, _ string) common.Executor {
	return func(_ context.Context) error {
		return nil
	}
}

func NewDockerNetworkDisconnectExecutor(_, _ string) common.Executor {
	return func(_ context.Context) error {
		return nil
	}
}

func RemoveOrphanNetworks(_ context.Context, _, _ string, _ time.Time) error {
	return nil
}

func CreateJobVolumes(ctx context.Context, runnerUUID string, volumeNames []string) error {
	return nil
}

func RemoveOrphanJobVolumes(ctx context.Context, runnerUUID string, createdBefore time.Time) error {
	return nil
}
