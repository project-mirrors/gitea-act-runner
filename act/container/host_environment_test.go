// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

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
	"testing/synctest"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ ExecutionsEnvironment = &HostEnvironment{}

func TestActionPlatformNames(t *testing.T) {
	assert.Equal(t, []string{"Linux", "macOS", "Windows", "FreeBSD"}, []string{goOsToActionOs("linux"), goOsToActionOs("darwin"), goOsToActionOs("windows"), goOsToActionOs("freebsd")})
	assert.Equal(t, []string{"X86", "X64", "ARM", "ARM64"}, []string{goArchToActionArch("386"), goArchToActionArch("amd64"), goArchToActionArch("arm"), goArchToActionArch("arm64")})
}

func TestCopyDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "action")
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".git", "objects"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git", "objects", "pack"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "action.yml"), []byte("name: x\n"), 0o600))

	for _, test := range []struct {
		name                     string
		useGitIgnore, skipGitDir bool
	}{
		{name: "git dir kept for a workspace copy"},
		{name: "git dir skipped for an action copy", skipGitDir: true},
		{name: "unfiltered outside a git repository", useGitIgnore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "dest")
			require.NoError(t, (&HostEnvironment{}).CopyDir(dest, src+string(filepath.Separator), test.useGitIgnore, test.skipGitDir)(t.Context()))

			assert.FileExists(t, filepath.Join(dest, "action.yml"))
			if test.skipGitDir {
				assert.NoDirExists(t, filepath.Join(dest, ".git"))
			} else {
				assert.FileExists(t, filepath.Join(dest, ".git", "objects", "pack"))
			}
		})
	}
}

func TestGetContainerArchive(t *testing.T) {
	ctx := t.Context()
	e := &HostEnvironment{Path: t.TempDir()}
	expectedContent := []byte("sdde/7sh")
	err := os.WriteFile(filepath.Join(e.Path, "action.yml"), expectedContent, 0o600)
	require.NoError(t, err)
	archive, err := e.GetContainerArchive(ctx, e.Path)
	require.NoError(t, err)
	defer archive.Close()
	reader := tar.NewReader(archive)
	h, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, "action.yml", h.Name)
	content, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, expectedContent, content)
	_, err = reader.Next()
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, archive.Close())

	_, err = e.GetContainerArchive(ctx, filepath.Join(e.Path, "missing"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestHostEnvironmentExecExitCode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	for name, allocatePTY := range map[string]bool{"without PTY": false, "with PTY": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				e := &HostEnvironment{Path: t.TempDir(), StdOut: writer, AllocatePTY: allocatePTY}
				t.Cleanup(func() { assert.NoError(t, e.Remove()(t.Context())) })

				result := make(chan error, 1)
				go func() {
					defer writer.Close()
					result <- e.Exec([]string{"sh", "-c", "printf 'failed\\n'; exit 3"}, map[string]string{"PATH": os.Getenv("PATH")}, "", "")(t.Context())
				}()
				first := make([]byte, 1)
				_, err := io.ReadFull(reader, first)
				require.NoError(t, err)
				synctest.Wait()
				select {
				case err := <-result:
					t.Fatalf("Exec returned before its output was consumed: %v", err)
				default:
				}
				remaining, err := io.ReadAll(reader)
				require.NoError(t, err)
				err = <-result
				var exitErr ExitCodeError
				require.ErrorAs(t, err, &exitErr)
				assert.Equal(t, ExitCodeError(3), exitErr)
				assert.Equal(t, "Process completed with exit code 3.", err.Error())
				assert.Equal(t, "failed\n", string(first)+string(remaining))
			})
		})
	}
}

func TestHostEnvironmentAllocatePTY(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	for _, tc := range []struct {
		name     string
		allocPTY bool
		expect   string
	}{
		{name: "off", allocPTY: false, expect: "NOTTY"},
		{name: "on", allocPTY: true, expect: "TTY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buf := &bytes.Buffer{}
			e := &HostEnvironment{
				Path:        t.TempDir(),
				StdOut:      buf,
				AllocatePTY: tc.allocPTY,
			}

			err := e.Exec(
				[]string{"sh", "-c", "[ -t 1 ] && printf TTY || printf NOTTY"},
				map[string]string{"PATH": os.Getenv("PATH")}, "", "",
			)(t.Context())
			require.NoError(t, err)
			got := strings.TrimSpace(strings.ReplaceAll(buf.String(), "\r", ""))
			assert.Equal(t, tc.expect, got)
		})
	}
}

func TestHostEnvironmentRemoveWorkdir(t *testing.T) {
	miscRoot, workdir := t.TempDir(), t.TempDir()
	e := &HostEnvironment{Path: filepath.Join(miscRoot, "hostexecutor"), Workdir: workdir, Root: miscRoot}
	require.NoError(t, os.MkdirAll(e.Path, 0o700))
	require.NoError(t, e.Remove()(t.Context()))
	assert.NoDirExists(t, miscRoot)
	assert.DirExists(t, workdir)
}

func TestRemoveAllWithContextDoesNotHangOnStuckDelete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		stubDone := make(chan struct{})

		orig := removeAll
		removeAll = func(string) error {
			defer close(stubDone)
			<-release
			return nil
		}
		t.Cleanup(func() {
			close(release)
			<-stubDone
			removeAll = orig
		})

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		err := removeAllWithContext(ctx, t.TempDir())
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestHostEnvironmentRemoveDoesNotHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origTimeout, origRemoveAll := hostCleanupTimeout, removeAll
		hostCleanupTimeout = 100 * time.Millisecond
		release, stubDone := make(chan struct{}), make(chan struct{})
		removeAll = func(string) error {
			defer close(stubDone)
			<-release
			return nil
		}
		t.Cleanup(func() {
			close(release)
			<-stubDone
			hostCleanupTimeout, removeAll = origTimeout, origRemoveAll
		})
		e := &HostEnvironment{Path: filepath.Join(t.TempDir(), "misc", "hostexecutor"), StdOut: os.Stdout}
		require.NoError(t, os.MkdirAll(e.Path, 0o700))
		done := make(chan error, 1)
		go func() { done <- e.Remove()(common.WithLogger(t.Context(), logging.Default())) }()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("Remove() hung on a stuck path removal")
		}
	})
}

func TestJoinPath(t *testing.T) {
	assert.Equal(t, "//server/share/act/workflow/envs.txt", JoinPath("//server/share/act", "workflow/envs.txt"))
	assert.Equal(t, "/var/run/act/workflow/envs.txt", JoinPath("/var/run/act/", "workflow", "envs.txt"))
	for _, root := range []string{"//server/share/act", "/var/run/act", "C:/act"} {
		assert.Equal(t, root+"/actions/key/main.go.out", JoinPath(root+"/actions/key", "main.go.out"))
		assert.Equal(t, root+"/workflow", JoinPath(root+"/workflow/outputcmd.txt", ".."))
	}
}
