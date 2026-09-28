// Copyright 2024 The Gitea Authors. All rights reserved.
// Copyright 2024 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package filecollector

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIgnoredTrackedfile(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "mygitrepo")
	repo, err := git.PlainInit(repoDir, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(".*\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".env"), []byte("test=val1\n"), 0o644))
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add(".gitignore")
	require.NoError(t, err)

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	patterns, err := gitignore.ReadPatterns(worktree.Filesystem, nil)
	require.NoError(t, err)
	ignorer := gitignore.NewMatcher(patterns)
	fc := &FileCollector{
		Ignorer:   ignorer,
		SrcPath:   repoDir,
		SrcPrefix: repoDir + string(filepath.Separator),
		Handler: &TarCollector{
			TarWriter: tw,
		},
	}
	err = filepath.Walk(repoDir, fc.CollectFiles(context.Background(), nil))
	assert.NoError(t, err, "successfully collect files")
	require.NoError(t, tw.Close())
	tr := tar.NewReader(&archive)
	h, err := tr.Next()
	assert.NoError(t, err, "tar must not be empty") //nolint:testifylint // pre-existing issue from nektos/act
	assert.Equal(t, ".gitignore", h.Name)
	_, err = tr.Next()
	assert.ErrorIs(t, err, io.EOF, "tar must only contain one element")
}

func TestSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires elevated privileges on Windows")
	}
	repoDir := filepath.Join(t.TempDir(), "mygitrepo")
	repo, err := git.PlainInit(repoDir, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".env"), []byte("test=val1\n"), 0o644))
	require.NoError(t, os.Symlink(".env", filepath.Join(repoDir, "test.env")))
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add("test.env")
	require.NoError(t, err)

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	fc := &FileCollector{
		SrcPath:   repoDir,
		SrcPrefix: repoDir + string(filepath.Separator),
		Handler: &TarCollector{
			TarWriter: tw,
		},
	}
	err = filepath.Walk(repoDir, fc.CollectFiles(context.Background(), nil))
	assert.NoError(t, err, "successfully collect files")
	require.NoError(t, tw.Close())
	tr := tar.NewReader(&archive)
	h, err := tr.Next()
	files := map[string]tar.Header{}
	for err == nil {
		files[h.Name] = *h
		h, err = tr.Next()
	}

	assert.Equal(t, ".env", files[".env"].Name)
	assert.Equal(t, "test.env", files["test.env"].Name)
	assert.Equal(t, ".env", files["test.env"].Linkname)
	assert.ErrorIs(t, err, io.EOF, "tar must be read cleanly to EOF")
}

// Regression for https://gitea.com/gitea/runner/issues/876 and /941:
// re-copying an action directory must overwrite a pre-existing read-only
// file (e.g. a git pack .idx at mode 0444) instead of failing with EACCES
// on macOS or "Access is denied" on Windows.
func TestCopyCollectorWriteFileOverwritesReadOnlyFile(t *testing.T) {
	dst := t.TempDir()
	target := filepath.Join(dst, "sub", "pack.idx")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o444))

	src := filepath.Join(t.TempDir(), "pack.idx")
	require.NoError(t, os.WriteFile(src, []byte("new"), 0o444))
	fi, err := os.Stat(src)
	require.NoError(t, err)

	cc := &CopyCollector{DstDir: dst}
	require.NoError(t, cc.WriteFile("sub/pack.idx", fi, "", strings.NewReader("new")))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}

// Without the destination removal, os.Symlink fails with EEXIST when the
// path already holds a regular file from an earlier copy of the action.
func TestCopyCollectorWriteFileOverwritesFileWithSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires elevated privileges on Windows")
	}
	dst := t.TempDir()
	target := filepath.Join(dst, "link")
	require.NoError(t, os.WriteFile(target, []byte("stale"), 0o644))

	fi, err := os.Lstat(target)
	require.NoError(t, err)

	cc := &CopyCollector{DstDir: dst}
	require.NoError(t, cc.WriteFile("link", fi, "target", nil))

	resolved, err := os.Readlink(target)
	require.NoError(t, err)
	assert.Equal(t, "target", resolved)
}

func TestFileCollectorCancellationAndWalkError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	walk := (&FileCollector{}).CollectFiles(ctx, nil)

	err := walk("file", nil, nil)
	require.EqualError(t, err, "copy cancelled")

	err = walk("file", nil, os.ErrPermission)
	require.ErrorIs(t, err, os.ErrPermission)
}
