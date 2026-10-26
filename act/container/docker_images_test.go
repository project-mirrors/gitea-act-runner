// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"

	mobyclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	logging.SetLevel(logging.LevelDebug)
}

func loadScratchImage(t *testing.T, platform string) string {
	t.Helper()
	tag := fmt.Sprintf("act-test-exists-%s-%d:latest", strings.TrimPrefix(platform, "linux/"), time.Now().UnixNano())
	var archive bytes.Buffer
	require.NoError(t, writeFilesTar(t.Context(), &archive, 0, 0,
		&FileEntry{Name: "config.json", Mode: 0o600, Body: fmt.Sprintf(`{"architecture":%q,"os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`, strings.TrimPrefix(platform, "linux/"))},
		&FileEntry{Name: "manifest.json", Mode: 0o600, Body: fmt.Sprintf(`[{"Config":"config.json","RepoTags":[%q],"Layers":[]}]`, tag)},
	))
	cli, err := GetDockerClient(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	response, err := cli.ImageLoad(t.Context(), &archive)
	require.NoError(t, err)
	defer response.Close()
	require.NoError(t, logDockerResponse(common.Logger(t.Context()), response, false))
	t.Cleanup(func() {
		_, _ = cli.ImageRemove(context.WithoutCancel(t.Context()), tag, mobyclient.ImageRemoveOptions{Force: true})
	})
	return tag
}

func TestImageExistsLocally(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	ctx := t.Context()

	missing, err := ImageExistsLocally(ctx, "library/alpine:this-random-tag-will-never-exist", "linux/amd64")
	assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act
	assert.False(t, missing)

	amd64Ref := loadScratchImage(t, "linux/amd64")
	arm64Ref := loadScratchImage(t, "linux/arm64")

	amd64Exists, err := ImageExistsLocally(ctx, amd64Ref, "linux/amd64")
	assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act
	assert.True(t, amd64Exists)

	arm64Exists, err := ImageExistsLocally(ctx, arm64Ref, "linux/arm64")
	assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act
	assert.True(t, arm64Exists)

	wrongPlatform, err := ImageExistsLocally(ctx, amd64Ref, "linux/arm64")
	assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act
	assert.False(t, wrongPlatform)

	removed, err := RemoveImage(ctx, amd64Ref, false, false)
	require.NoError(t, err)
	assert.True(t, removed)
	removed, err = RemoveImage(ctx, amd64Ref, false, false)
	require.NoError(t, err)
	assert.False(t, removed)
}
