// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !WITHOUT_DOCKER && (linux || darwin || netbsd)

package container

import (
	"archive/tar"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateBuildContextKeepsHardlinkedSpecialFilesLinked(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "first"), 0o644))
	require.NoError(t, os.Link(filepath.Join(dir, "first"), filepath.Join(dir, "second")))

	buildContext, err := createBuildContext(t.Context(), dir, "Dockerfile")
	require.NoError(t, err)
	defer buildContext.Close()
	reader := tar.NewReader(buildContext)
	first, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, byte(tar.TypeFifo), first.Typeflag)
	second, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, byte(tar.TypeLink), second.Typeflag)
	assert.Equal(t, first.Name, second.Linkname)
}
