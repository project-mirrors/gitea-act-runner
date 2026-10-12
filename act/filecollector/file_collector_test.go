// Copyright 2024 The Gitea Authors. All rights reserved.
// Copyright 2024 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package filecollector

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gitCmd(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestCollectFilesKeepsWhatGitTracksOrDoesNotIgnore(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "mygitrepo")
	for _, dir := range []string{repoDir, filepath.Join(repoDir, "sub"), filepath.Join(repoDir, "nested")} {
		gitCmd(t, "init", "-q", dir)
	}
	for name, body := range map[string]string{
		".gitignore":     ".*\nign/\n",
		".env":           "",
		"Renamed":        "",
		"ign/tracked":    "",
		"ign/untracked":  "",
		"sub/.gitignore": "dropped\n",
		"sub/dropped":    "",
		"sub/kept":       "",
		"nested/kept":    "",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(repoDir, name)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(body), 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(repoDir, "uninitialized"), 0o755))
	gitCmd(t, "-C", repoDir, "add", "-f", ".gitignore", "Renamed", "ign/tracked")
	require.NoError(t, os.Rename(filepath.Join(repoDir, "Renamed"), filepath.Join(repoDir, "renamed")))
	for _, gitlink := range []string{"sub", "uninitialized"} {
		gitCmd(t, "-C", repoDir, "update-index", "--add", "--cacheinfo", "160000,4b825dc642cb6eb9a060e54bf8d69288fbee4904,"+gitlink)
	}
	expected := map[string]string{".gitignore": "", "ign/tracked": "", "nested/kept": "", "renamed": "", "sub/.gitignore": "", "sub/kept": ""}
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Symlink(".env", filepath.Join(repoDir, "test.env")))
		expected["test.env"] = ".env"
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	fc := &FileCollector{SrcPath: repoDir, SrcPrefix: repoDir + string(filepath.Separator), UseGitIgnore: true, Handler: &TarCollector{TarWriter: tw}}
	require.NoError(t, filepath.Walk(repoDir, fc.CollectFiles(t.Context())))
	require.NoError(t, tw.Close())

	files := map[string]string{}
	tr := tar.NewReader(&archive)
	for header, err := tr.Next(); err != io.EOF; header, err = tr.Next() {
		require.NoError(t, err)
		files[header.Name] = header.Linkname
	}
	assert.Contains(t, files, ".git/HEAD")
	maps.DeleteFunc(files, func(name, _ string) bool { return strings.Contains("/"+name, "/.git/") })
	assert.Equal(t, expected, files)
}

func TestCopyCollectorWriteFileOverwritesReadOnlyFile(t *testing.T) {
	dst := t.TempDir()
	target := filepath.Join(dst, "sub", "pack.idx")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o444))
	fi, err := os.Stat(target)
	require.NoError(t, err)

	cc := &CopyCollector{DstDir: dst}
	require.NoError(t, cc.WriteFile("sub/pack.idx", fi, "", strings.NewReader("new")))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}

func TestCopyCollectorWriteFileOverwritesFileWithSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires elevated privileges on Windows")
	}
	dst := t.TempDir()
	target := filepath.Join(dst, "link")
	require.NoError(t, os.WriteFile(target, []byte("stale"), 0o644))

	cc := &CopyCollector{DstDir: dst}
	require.NoError(t, cc.WriteFile("link", nil, "target", nil))

	resolved, err := os.Readlink(target)
	require.NoError(t, err)
	assert.Equal(t, "target", resolved)
}

func TestFileCollectorCancellationAndWalkError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	walk := (&FileCollector{}).CollectFiles(ctx)

	err := walk("file", nil, nil)
	require.EqualError(t, err, "copy cancelled")

	err = walk("file", nil, os.ErrPermission)
	require.ErrorIs(t, err, os.ErrPermission)
}
