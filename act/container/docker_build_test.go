// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateBuildContextFollowsDockerignoreAndSymlinkedRootButKeepsDockerfile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		".dockerignore": "*.log\nbuild\n!build/keep.txt\nDockerfile", "Dockerfile": "FROM scratch", "a.txt": "a",
		"a.log": "log", "build/drop.txt": "drop", "build/keep.txt": "keep",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "empty"), 0o755))

	contexts := []string{dir}
	if link := filepath.Join(t.TempDir(), "link"); os.Symlink(dir, link) == nil {
		contexts = append(contexts, link+string(filepath.Separator))
	}

	for _, contextDir := range contexts {
		buildContext, err := createBuildContext(t.Context(), contextDir, "Dockerfile")
		require.NoError(t, err)
		var names []string
		for reader := tar.NewReader(buildContext); ; {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			names = append(names, header.Name)
		}
		require.NoError(t, buildContext.Close())
		require.Equal(t, []string{".dockerignore", "a.txt", "build/keep.txt", "empty/", "Dockerfile"}, names, contextDir)
	}
}
