// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageExistsLocally returns a boolean indicating if an image with the
// requested name, tag and architecture exists in the local docker image store
func ImageExistsLocally(ctx context.Context, imageName, platform string) (bool, error) {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return false, err
	}
	defer cli.Close()

	inspectImage, err := cli.ImageInspect(ctx, imageName)
	if cerrdefs.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	if platform == "" || platform == "any" {
		return true, nil
	}

	requested, err := parsePlatform(platform)
	if err != nil {
		return false, err
	}
	return platformMatches(requested, &specs.Platform{OS: inspectImage.Os, Architecture: inspectImage.Architecture, Variant: inspectImage.Variant}), nil
}

func platformMatches(requested, image *specs.Platform) bool {
	variant := image.Variant
	if variant == "" {
		variant = map[string]string{"amd64": "v1", "arm": "v7", "arm64": "v8"}[image.Architecture]
	}
	return image.OS == requested.OS && image.Architecture == requested.Architecture && (requested.Variant == "" || requested.Variant == variant)
}

// RemoveImage removes image from local store, the function is used to run different
// container image architectures
func RemoveImage(ctx context.Context, imageName string, force, pruneChildren bool) (bool, error) {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return false, err
	}
	defer cli.Close()

	if _, err := cli.ImageInspect(ctx, imageName); cerrdefs.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	if _, err = cli.ImageRemove(ctx, imageName, client.ImageRemoveOptions{
		Force:         force,
		PruneChildren: pruneChildren,
	}); err != nil {
		return false, err
	}

	return true, nil
}
