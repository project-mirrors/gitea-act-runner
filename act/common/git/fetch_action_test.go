// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchAction(t *testing.T) {
	original := archiveRetryDelay
	archiveRetryDelay = func(_retryAfter string) time.Duration { return time.Nanosecond }
	t.Cleanup(func() { archiveRetryDelay = original })

	root := t.TempDir()
	remote := filepath.Join(root, "org", "action")
	require.NoError(t, gitCmd("init", "--initial-branch=main", remote))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "action.yml"), []byte("name: action\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Symlink("action.yml", filepath.Join(remote, "link")))
	require.NoError(t, gitCmd("-C", remote, "add", "."))
	require.NoError(t, gitCmd("-C", remote, "commit", "-m", "initial"))
	require.NoError(t, gitCmd("-C", remote, "tag", "v1"))

	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var mu sync.Mutex
	var archiveRequests []http.Header
	var mirror *httptest.Server
	requests, archives, failures := 0, "", 0
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		ref, ok := strings.CutPrefix(request.URL.Path, "/org/action/archive/")
		if !ok {
			backend.ServeHTTP(writer, request)
			return
		}
		archiveRequests = append(archiveRequests, request.Header.Clone())
		switch {
		case failures > 0:
			failures--
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		case archives == "none":
			http.NotFound(writer, request)
			return
		case archives == "unreachable":
			http.Redirect(writer, request, "http://127.0.0.1:1"+request.URL.Path, http.StatusFound)
			return
		case archives == "redirect" && request.Host != mirror.Listener.Addr().String():
			http.Redirect(writer, request, mirror.URL+request.URL.Path, http.StatusFound)
			return
		}
		sha, _ := gitOutput(request.Context(), remote, "rev-parse", strings.TrimSuffix(ref, ".tar.gz"))
		etag := fmt.Sprintf(`W/"%s-tar.gz-%t"`, sha, archives != "unprefixed")
		if request.Header.Get("If-None-Match") == etag {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		args := []string{"archive", "--format=tar.gz", sha}
		if archives != "unprefixed" {
			args = append(args, "--prefix=action/")
		}
		archive, _ := runGit(request.Context(), remote, nil, args...)
		writer.Header().Set("ETag", etag)
		_, _ = writer.Write(archive)
	})
	server := httptest.NewServer(handler)
	mirror = httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Cleanup(mirror.Close)
	serve := func(mode string, failing int) {
		mu.Lock()
		defer mu.Unlock()
		requests, archives, failures, archiveRequests = 0, mode, failing, nil
	}
	served := func() (int, []http.Header) {
		mu.Lock()
		defer mu.Unlock()
		return requests, append([]http.Header(nil), archiveRequests...)
	}
	newInput := func(ref string) NewGitCloneExecutorInput {
		return NewGitCloneExecutorInput{URL: server.URL + "/org/action", Ref: ref, Dir: t.TempDir(), Token: "secret"}
	}

	t.Run("downloads, revalidates and follows a moved tag", func(t *testing.T) {
		serve("prefixed", 0)
		input := newInput("v1")
		tree, sha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, gitRevParse(t, remote, "v1"), sha)
		assert.Equal(t, filepath.Join(input.Dir, sha), tree)
		assert.NoDirExists(t, filepath.Join(tree, ".git"))
		info, err := os.Stat(filepath.Join(tree, "run.sh"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
		link, err := os.Readlink(filepath.Join(tree, "link"))
		require.NoError(t, err)
		assert.Equal(t, "action.yml", link)

		again, _, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, tree, again)

		require.NoError(t, gitCmd("-C", remote, "commit", "--allow-empty", "-m", "moved"))
		require.NoError(t, gitCmd("-C", remote, "tag", "--force", "v1"))
		moved, movedSha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, gitRevParse(t, remote, "v1"), movedSha)
		assert.NotEqual(t, tree, moved)
		assert.FileExists(t, filepath.Join(tree, "action.yml"))

		input.OfflineMode = true
		_, offlineSha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, movedSha, offlineSha)

		requests, headers := served()
		assert.Equal(t, 3, requests)
		require.Len(t, headers, 3)
		assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("token:secret")), headers[0].Get("Authorization"))
		assert.Empty(t, headers[0].Get("If-None-Match"))
		assert.Equal(t, fmt.Sprintf(`W/"%s-tar.gz-true"`, sha), headers[1].Get("If-None-Match"))
		assert.Equal(t, headers[1].Get("If-None-Match"), headers[2].Get("If-None-Match"))
	})

	t.Run("downloads a pinned commit once and rejects a short one", func(t *testing.T) {
		serve("prefixed", 0)
		sha := gitRevParse(t, remote, "main")
		input := newInput(sha)
		for range 2 {
			tree, got, err := FetchAction(t.Context(), input)
			require.NoError(t, err)
			assert.Equal(t, sha, got)
			assert.FileExists(t, filepath.Join(tree, "action.yml"))
		}
		_, _, err := FetchAction(t.Context(), newInput(sha[:7]))
		require.ErrorIs(t, err, ErrShortRef)
		_, headers := served()
		assert.Len(t, headers, 1)
		assert.NoDirExists(t, filepath.Join(input.Dir, "git"))
	})

	t.Run("retries a failing download and drops the token on a redirect to another origin", func(t *testing.T) {
		serve("redirect", 2)
		input := newInput("main")
		_, sha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, gitRevParse(t, remote, "main"), sha)
		assert.NoDirExists(t, filepath.Join(input.Dir, "git"))
		_, headers := served()
		require.Len(t, headers, 4)
		assert.NotEmpty(t, headers[2].Get("Authorization"))
		assert.Empty(t, headers[3].Get("Authorization"))
	})

	t.Run("keeps an unprefixed archive whole, downloads where insteadOf points and exports a clone without an archive or from an unreachable host", func(t *testing.T) {
		for _, mode := range []string{"unprefixed", "none", "unreachable"} {
			serve(mode, 0)
			tree, sha, err := FetchAction(t.Context(), newInput("main"))
			require.NoError(t, err, mode)
			assert.Equal(t, gitRevParse(t, remote, "main"), sha, mode)
			assert.FileExists(t, filepath.Join(tree, "action.yml"), mode)
			assert.NoDirExists(t, filepath.Join(tree, ".git"), mode)
			_, headers := served()
			assert.Len(t, headers, 1, mode)
		}

		serve("prefixed", 0)
		config := filepath.Join(t.TempDir(), "gitconfig")
		require.NoError(t, os.WriteFile(config, []byte("[url \""+server.URL+"/org/\"]\n\tinsteadOf = https://example.invalid/org/\n"), 0o644))
		t.Setenv("GIT_CONFIG_GLOBAL", config)
		input := newInput("main")
		input.URL = "https://example.invalid/org/action"
		_, sha, err := FetchAction(t.Context(), input)
		require.NoError(t, err)
		assert.Equal(t, gitRevParse(t, remote, "main"), sha)
		_, headers := served()
		assert.Len(t, headers, 1)
		assert.NoDirExists(t, filepath.Join(input.Dir, "git"))
	})
}

func TestExtractTreeConfinesEntries(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]*tar.Header{
		"parent path":       {{Name: "../escape", Typeflag: tar.TypeReg}},
		"through a symlink": {{Name: "out", Typeflag: tar.TypeSymlink, Linkname: outside}, {Name: "out/escape", Typeflag: tar.TypeReg}},
	} {
		t.Run(name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			require.NoError(t, writer.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)}}))
			for _, entry := range entries {
				require.NoError(t, writer.WriteHeader(entry))
			}
			require.NoError(t, writer.Close())

			dir := t.TempDir()
			_, err := extractTree(&archive, dir, false)
			require.Error(t, err)
			assert.NoFileExists(t, filepath.Join(dir, "escape"))
			assert.NoFileExists(t, filepath.Join(outside, "escape"))
		})
	}
}
