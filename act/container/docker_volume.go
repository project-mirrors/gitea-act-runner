// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gitea.com/gitea/runner/act/common"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

func CreateJobVolumes(ctx context.Context, runnerUUID string, volumeNames []string) error {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()

	for _, volumeName := range volumeNames {
		if _, err := cli.VolumeCreate(ctx, client.VolumeCreateOptions{
			Name:   volumeName,
			Labels: runnerLabels(runnerUUID),
		}); err != nil {
			return err
		}
	}
	return nil
}

func RemoveOrphanJobVolumes(ctx context.Context, runnerUUID string, createdBefore time.Time) error {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to the docker daemon: %w", err)
	}
	defer cli.Close()

	volumes, err := cli.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", runnerUUIDLabel+"="+runnerUUID).Add("dangling", "true"),
	})
	if err != nil {
		return err
	}

	var errs []error
	for _, item := range volumes.Items {
		created, err := time.Parse(time.RFC3339, item.CreatedAt)
		// an unreadable or recent timestamp may belong to a job that is still starting up
		if err != nil || created.After(createdBefore) || item.Labels[runnerUUIDLabel] != runnerUUID {
			continue
		}
		if _, err := cli.VolumeRemove(ctx, item.Name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove volume %s: %w", item.Name, err))
			continue
		}
		common.Logger(ctx).Infof("removed docker volume %s left behind by an earlier job", item.Name)
	}
	return errors.Join(errs...)
}

func NewDockerVolumeRemoveExecutor(volumeName string, force bool) common.Executor {
	return func(ctx context.Context) error {
		common.Logger(ctx).Debugf("docker volume rm %s", volumeName)

		if common.Dryrun(ctx) {
			return nil
		}

		cli, err := GetDockerClient(ctx)
		if err != nil {
			return err
		}
		defer cli.Close()

		_, err = cli.VolumeRemove(ctx, volumeName, client.VolumeRemoveOptions{Force: force})
		if cerrdefs.IsNotFound(err) { // already gone is the outcome we wanted
			return nil
		}
		return err
	}
}
