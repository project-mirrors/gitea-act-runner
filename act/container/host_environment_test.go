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
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ ExecutionsEnvironment = &HostEnvironment{}

func TestActionPlatformNames(t *testing.T) {
	assert.Equal(t, []string{"Linux", "macOS", "Windows"}, []string{goOsToActionOs("linux"), goOsToActionOs("darwin"), goOsToActionOs("windows")})
	assert.Equal(t, []string{"X86", "X64", "ARM", "ARM64"}, []string{goArchToActionArch("386"), goArchToActionArch("amd64"), goArchToActionArch("arm"), goArchToActionArch("arm64")})
}

func TestCopyDir(t *testing.T) {
	for _, test := range []struct {
		name                     string
		useGitIgnore, skipGitDir bool
	}{
		{name: "git dir kept for a workspace copy"},
		{name: "git dir skipped for an action copy", skipGitDir: true},
		{name: "unfiltered outside a git repository", useGitIgnore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "action")
			require.NoError(t, os.MkdirAll(filepath.Join(src, ".git", "objects"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(src, ".git", "objects", "pack"), []byte("x"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(src, "action.yml"), []byte("name: x\n"), 0o600))

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
	assert.ErrorIs(t, err, io.EOF)
}

func TestHostEnvironmentExecExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	for name, allocatePTY := range map[string]bool{"without PTY": false, "with PTY": true} {
		t.Run(name, func(t *testing.T) {
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
			select {
			case err := <-result:
				t.Fatalf("Exec returned before its output was consumed: %v", err)
			case <-time.After(100 * time.Millisecond):
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
	}
}

func TestHostEnvironmentAllocatePTY(t *testing.T) {
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
	for name, owned := range map[string]bool{"preserves caller workdir": false, "cleans owned workdir": true} {
		t.Run(name, func(t *testing.T) {
			miscRoot, workdir := t.TempDir(), t.TempDir()
			e := &HostEnvironment{
				Path:         filepath.Join(miscRoot, "hostexecutor"),
				Workdir:      workdir,
				CleanWorkdir: owned,
				CleanUp:      func() { _ = os.RemoveAll(miscRoot) },
			}
			require.NoError(t, os.MkdirAll(e.Path, 0o700))
			require.NoError(t, e.Remove()(t.Context()))
			_, err := os.Stat(workdir)
			if owned {
				assert.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRemoveAllWithContextDoesNotHangOnStuckDelete(t *testing.T) {
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
}

func TestHostEnvironmentRemoveDoesNotHang(t *testing.T) {
	for _, phase := range []string{"CleanUp callback", "path removal"} {
		t.Run(phase, func(t *testing.T) {
			origTimeout := hostCleanupTimeout
			hostCleanupTimeout = 100 * time.Millisecond
			t.Cleanup(func() { hostCleanupTimeout = origTimeout })
			release, stubDone := make(chan struct{}), make(chan struct{})
			block := func() {
				defer close(stubDone)
				<-release
			}
			e := &HostEnvironment{Path: filepath.Join(t.TempDir(), "misc", "hostexecutor"), StdOut: os.Stdout}
			if phase == "CleanUp callback" {
				e.CleanUp = block
			} else {
				origRemoveAll := removeAll
				removeAll = func(string) error {
					block()
					return nil
				}
				t.Cleanup(func() { removeAll = origRemoveAll })
			}
			t.Cleanup(func() {
				close(release)
				<-stubDone
			})
			require.NoError(t, os.MkdirAll(e.Path, 0o700))
			done := make(chan error, 1)
			go func() { done <- e.Remove()(common.WithLogger(t.Context(), logging.Default())) }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatalf("Remove() hung on a stuck %s", phase)
			}
		})
	}
}

func TestBuildWindowsWorkspaceKillScript(t *testing.T) {
	t.Run("single dir", func(t *testing.T) {
		s := buildWindowsWorkspaceKillScript([]string{`C:\workspace\job1`})
		assert.Contains(t, s, `$paths = @('C:\workspace\job1')`)
		assert.Contains(t, s, "$selfPid = $PID")
		assert.Contains(t, s, "$_.ProcessId -eq $selfPid")
		assert.Contains(t, s, `$prefix = $p + '\'`)
		assert.Contains(t, s, "$_.ExecutablePath.StartsWith($prefix")
		assert.Contains(t, s, "$_.CommandLine.IndexOf($prefix")
		assert.Contains(t, s, "taskkill.exe /PID $_.ProcessId /T /F")
	})

	t.Run("multiple dirs comma-separated", func(t *testing.T) {
		s := buildWindowsWorkspaceKillScript([]string{
			`C:\work\path`,
			`C:\work\workdir`,
			`C:\Users\runner\AppData\Local\Temp\job-42`,
		})
		assert.Contains(t, s, `'C:\work\path'`)
		assert.Contains(t, s, `'C:\work\workdir'`)
		assert.Contains(t, s, `'C:\Users\runner\AppData\Local\Temp\job-42'`)
		assert.Contains(t, s, `'C:\work\path','C:\work\workdir',`)
	})

	t.Run("path with single quote is escaped", func(t *testing.T) {
		s := buildWindowsWorkspaceKillScript([]string{`C:\work\it's\path`})
		assert.Contains(t, s, `'C:\work\it''s\path'`)
		assert.NotContains(t, s, `'C:\work\it's\path'`)
	})

	t.Run("path with wildcard metacharacters is matched literally", func(t *testing.T) {
		s := buildWindowsWorkspaceKillScript([]string{`C:\work\[job]?1`})
		assert.Contains(t, s, `'C:\work\[job]?1'`)
		assert.NotContains(t, s, "-like")
		assert.Contains(t, s, "StartsWith")
		assert.Contains(t, s, "IndexOf")
	})

	t.Run("empty dir list still produces a valid script", func(t *testing.T) {
		s := buildWindowsWorkspaceKillScript(nil)
		assert.Contains(t, s, "$paths = @()")
		assert.Contains(t, s, "Get-CimInstance Win32_Process")
	})
}
