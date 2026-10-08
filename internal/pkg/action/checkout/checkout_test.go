// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package checkout

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func gitLog(dir string) []string {
	out, err := exec.Command("git", "--git-dir", filepath.Join(dir, ".git"), "log", "--format=%s").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

func TestCheckout(t *testing.T) {
	server := t.TempDir()
	var sha string
	_, lfsErr := exec.LookPath("git-lfs")
	require.True(t, t.Run("fixtures", func(t *testing.T) {
		t.Run("history", func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			runGit(t, "init", "--initial-branch=main", work)
			runGit(t, "-C", work, "commit", "--allow-empty", "-m", "c1")
			sha = runGit(t, "-C", work, "rev-parse", "HEAD")
			runGit(t, "-C", work, "tag", "v1")
			runGit(t, "-C", work, "commit", "--allow-empty", "-m", "c2")
			runGit(t, "-C", work, "checkout", "-b", "feature")
			runGit(t, "-C", work, "commit", "--allow-empty", "-m", "f1")
			runGit(t, "-C", work, "checkout", "--orphan", "trunk")
			runGit(t, "-C", work, "commit", "--allow-empty", "-m", "t1")
			runGit(t, "-C", work, "commit", "--allow-empty", "-m", "t2")
			runGit(t, "-C", work, "tag", "trunk", "main")
			runGit(t, "clone", "--bare", "--branch", "main", work, filepath.Join(server, "org", "repo"))
			runGit(t, "clone", "--bare", "--branch", "trunk", work, filepath.Join(server, "other", "repo"))
			runGit(t, "-C", filepath.Join(server, "other", "repo"), "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/main")
		})
		t.Run("sha256", func(t *testing.T) {
			t.Parallel()
			sha256Repo := filepath.Join(server, "org", "sha256")
			runGit(t, "init", "--object-format=sha256", "--initial-branch=main", sha256Repo)
			runGit(t, "-C", sha256Repo, "commit", "--allow-empty", "-m", "s1")
			runGit(t, "-C", sha256Repo, "checkout", "--detach")
		})
		t.Run("submodules", func(t *testing.T) {
			t.Parallel()
			publish(t, server, "org/nested", map[string]string{"nested.txt": "nested"}, nil)
			publish(t, server, "org/child", map[string]string{"child.txt": "child"}, map[string]string{"nested": "../nested"})
			publish(t, server, "org/content", map[string]string{"root.txt": "root", "a/a.txt": "a", "b/b.txt": "b"}, map[string]string{"sub": "../child"})
			runGit(t, "-C", filepath.Join(server, "org", "content"), "config", "uploadpack.allowFilter", "true")
		})
		t.Run("lfs", func(t *testing.T) {
			t.Parallel()
			oid := fmt.Sprintf("%x", sha256.Sum256([]byte("big")))
			pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 3\n"
			publish(t, server, "org/lfs", map[string]string{".gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n", "keep/big.bin": pointer, "drop/big.bin": pointer}, nil)
			object := filepath.Join(server, "org", "lfs", "lfs", "objects", oid[:2], oid[2:4], oid)
			require.NoError(t, os.MkdirAll(filepath.Dir(object), 0o755))
			require.NoError(t, os.WriteFile(object, []byte("big"), 0o644))
		})
	}))
	host := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(host, "marker"), []byte("marker"), 0o644))
	tools := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tools, "ssh"), []byte(`#!/bin/sh
test -s "$2" || exit 1
printf '%s\n' "$@" > "$SSH_ARGS"
for last; do :; done
exec sh -c "$(printf '%s' "$last" | sed "s#git-upload-pack '/\(.*\)\.git'#git upload-pack '$SSH_SERVER/\1'#")"
`), 0o755))
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[protocol \"file\"]\n\tallow = always\n"), 0o644))
	sshEvent := func(url string) map[string]any { return map[string]any{"repository": map[string]any{"ssh_url": url}} }

	for _, tc := range []struct {
		name           string
		bind, noSkip   bool
		eventRef       string
		event          map[string]any
		before, inputs map[string]string
		dst, head, err string
		log, refs      []string
		files, config  map[string]string
	}{
		{name: "bound workspace is skipped", bind: true},
		{name: "workflow ref copies host workdir", inputs: map[string]string{"ref": "main", "path": "a/../src"}, dst: "src", files: map[string]string{"marker": "marker"}},
		{name: "bound path checks out the event branch", bind: true, inputs: map[string]string{"path": "src"}, dst: "src", head: "main", log: []string{"c1"}},
		{name: "no-skip checks out the event branch", noSkip: true, head: "main", log: []string{"c1"}, refs: []string{"refs/remotes/origin/main"}},
		{name: "event tag is kept", noSkip: true, eventRef: "refs/tags/v1", log: []string{"c1"}, refs: []string{"refs/tags/v1"}},
		{name: "moved event tag fails", noSkip: true, eventRef: "refs/tags/trunk", err: "does not point to the expected commit " + sha},
		{name: "deleted event tag fails", noSkip: true, eventRef: "refs/tags/deleted", err: "checkout: clone"},
		{
			name: "sha input removes previously persisted credentials without clean", noSkip: true, before: map[string]string{"ref": sha},
			inputs: map[string]string{"ref": sha, "persist-credentials": "false", "clean": "false"},
			log:    []string{"c1"},
		},
		{name: "ref clones shallow", inputs: map[string]string{"ref": "refs/heads/feature"}, head: "feature", log: []string{"f1"}},
		{name: "tag ref wins over same-named branch and is kept", inputs: map[string]string{"ref": "refs/tags/trunk"}, log: []string{"c2"}, refs: []string{"refs/tags/trunk"}},
		{name: "branch ref wins over same-named tag", inputs: map[string]string{"ref": "refs/heads/trunk"}, head: "trunk", log: []string{"t2"}},
		{
			name: "short branch wins over same-named tag and fetch-tags fetches every tag", inputs: map[string]string{"ref": "trunk", "fetch-tags": "true"},
			head: "trunk", log: []string{"t2"}, refs: []string{"refs/tags/trunk", "refs/tags/v1"},
		},
		{name: "short tag", inputs: map[string]string{"ref": "v1"}, log: []string{"c1"}, refs: []string{"refs/tags/v1"}},
		{
			name: "fetch-depth 0 deepens a shallow checkout to every branch and tag", before: map[string]string{"ref": "feature"},
			inputs: map[string]string{"ref": "refs/heads/feature", "fetch-depth": "0"}, head: "feature", log: []string{"f1", "c2", "c1"},
			refs: []string{"refs/remotes/origin/feature", "refs/remotes/origin/main", "refs/remotes/origin/trunk", "refs/tags/trunk", "refs/tags/v1"},
		},
		{name: "other repository defaults to its default branch", inputs: map[string]string{"repository": "other/repo"}, head: "trunk", log: []string{"t2"}},
		{name: "explicit HEAD checks out the default branch", inputs: map[string]string{"repository": "other/repo", "ref": "HEAD"}, head: "trunk", log: []string{"t2"}},
		{name: "detached sha256 HEAD is checked out", inputs: map[string]string{"repository": "org/sha256"}, log: []string{"s1"}},
		{
			name: "ssh key with the advertised endpoint persists", event: sshEvent("ssh://svc@ssh.example:2222/org/repo.git"),
			inputs: map[string]string{"ssh-key": "KEY", "ssh-user": "me", "ssh-known-hosts": "ssh.example ssh-ed25519 AAAA"},
			head:   "main", log: []string{"c1"}, config: map[string]string{"remote.origin.url": "ssh://me@ssh.example:2222/org/repo.git"},
		},
		{
			name: "ssh key without persisting is emptied", event: sshEvent("svc@ssh.example:org/repo.git"),
			inputs: map[string]string{"ssh-key": "KEY", "persist-credentials": "false"},
			head:   "main", log: []string{"c1"}, config: map[string]string{"remote.origin.url": "ssh://svc@ssh.example/org/repo.git"},
		},
		{
			name: "sparse cone checkout filters blobs", inputs: map[string]string{"repository": "org/content", "sparse-checkout": "a\n", "show-progress": "false"},
			head: "main", log: []string{"content"}, files: map[string]string{"root.txt": "root", "a/a.txt": "a", "b/b.txt": ""},
			config: map[string]string{"remote.origin.partialclonefilter": "blob:none"},
		},
		{
			name: "non-cone sparse checkout with an explicit filter", head: "main", log: []string{"content"},
			inputs: map[string]string{"repository": "org/content", "sparse-checkout": "b/b.txt", "sparse-checkout-cone-mode": "false", "filter": "blob:limit=1024"},
			files:  map[string]string{"root.txt": "", "a/a.txt": "", "b/b.txt": "b"}, config: map[string]string{"remote.origin.partialclonefilter": "blob:limit=1024"},
		},
		{
			name: "full checkout after a sparse one restores every file", before: map[string]string{"repository": "org/content", "sparse-checkout": "a"},
			inputs: map[string]string{"repository": "org/content", "set-safe-directory": "false"}, head: "main", log: []string{"content"},
			files: map[string]string{"root.txt": "root", "a/a.txt": "a", "b/b.txt": "b", "sub/child.txt": ""},
		},
		{
			name: "recursive submodules after plain ones", before: map[string]string{"repository": "org/content", "submodules": "true"},
			inputs: map[string]string{"repository": "org/content", "submodules": "Recursive"}, head: "main", log: []string{"content"},
			files: map[string]string{"sub/child.txt": "child", "sub/nested/nested.txt": "nested"},
		},
		{
			name: "lfs content stays when narrowing a full checkout to a sparse one", before: map[string]string{"repository": "org/lfs", "lfs": "true"},
			inputs: map[string]string{"repository": "org/lfs", "lfs": "true", "sparse-checkout": "keep"}, head: "main", log: []string{"content"},
			files: map[string]string{"keep/big.bin": "big", "drop/big.bin": ""},
		},
		{name: "unknown ref", inputs: map[string]string{"ref": "missing"}, err: "has no ref"},
		{name: "unsupported input", inputs: map[string]string{"github-server-url": "https://example.com"}, err: "unsupported input"},
		{name: "relative path escaping workspace", inputs: map[string]string{"path": "../x"}, err: "must be relative"},
		{name: "absolute path", inputs: map[string]string{"path": "/x"}, err: "must be relative"},
		{name: "negative fetch-depth", inputs: map[string]string{"fetch-depth": "-1"}, err: "non-negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.inputs["lfs"] == "true" && lfsErr != nil {
				t.Skip("needs git-lfs")
			}
			workspace, runnerTemp := t.TempDir(), t.TempDir()
			output := filepath.Join(runnerTemp, "output")
			checkout := func(inputs map[string]string) error {
				return Main(t.Context(), &action.Context{
					Container: &container.HostEnvironment{Path: workspace, TmpDir: runnerTemp, StdOut: io.Discard},
					Github: &model.GithubContext{
						Repository: "org/repo", Ref: cmp.Or(tc.eventRef, "refs/heads/main"), Sha: sha, ServerURL: "file://" + server, Token: "tok", Event: tc.event,
					},
					Inputs:         inputs,
					HostWorkdir:    host,
					Workspace:      workspace,
					BindWorkdir:    tc.bind,
					NoSkipCheckout: tc.noSkip,
					Env: map[string]string{
						"PATH": tools + string(os.PathListSeparator) + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL": globalConfig, "GIT_CONFIG_NOSYSTEM": "1",
						"GIT_DIR": host, "GITHUB_OUTPUT": output, "SSH_SERVER": server, "SSH_ARGS": filepath.Join(runnerTemp, "ssh-args"),
					},
				})
			}
			dst := filepath.Join(workspace, tc.dst)
			if tc.err != "" {
				require.ErrorContains(t, checkout(tc.inputs), tc.err)
				if tc.eventRef == "" {
					entries, _ := os.ReadDir(workspace)
					assert.Empty(t, entries)
				} else {
					assert.Nil(t, gitLog(dst))
				}
				return
			}
			first := tc.before
			if first == nil {
				first = tc.inputs
			}
			require.NoError(t, checkout(first))
			repos := []string{dst}
			if fileExists(filepath.Join(dst, "sub", ".git")) {
				repos = append(repos, filepath.Join(dst, "sub"))
			}
			for _, repo := range repos {
				require.NoError(t, os.WriteFile(filepath.Join(repo, "untracked"), nil, 0o644))
			}
			require.NoError(t, checkout(tc.inputs))
			outputs, err := os.ReadFile(output)
			require.NoError(t, err)
			assert.Equal(t, tc.log, gitLog(dst))
			for name, content := range tc.files {
				body, err := os.ReadFile(filepath.Join(dst, name))
				assert.Equal(t, content, strings.TrimSpace(string(body)), name)
				assert.Equal(t, content == "", os.IsNotExist(err), name)
			}
			assert.Equal(t, tc.files["marker"] != "", fileExists(filepath.Join(dst, "marker")))
			for key, value := range tc.config {
				assert.Equal(t, value, gitOutput(dst, "config", key), key)
			}
			if tc.refs != nil {
				assert.Subset(t, strings.Fields(runGit(t, "-C", dst, "for-each-ref", "--format=%(refname)")), tc.refs)
			}
			if tc.log == nil {
				assert.Contains(t, string(outputs), "commit="+sha+"\n")
				assert.Contains(t, string(outputs), "ref="+cmp.Or(tc.eventRef, "refs/heads/main")+"\n")
				return
			}
			if tc.head == "" {
				assert.Empty(t, gitOutput(dst, "symbolic-ref", "-q", "HEAD"))
			} else {
				assert.Equal(t, "refs/heads/"+tc.head, gitOutput(dst, "symbolic-ref", "-q", "HEAD"))
				assert.Equal(t, "origin/"+tc.head, gitOutput(dst, "rev-parse", "--abbrev-ref", "@{u}"))
			}
			assert.Contains(t, string(outputs), "commit="+gitOutput(dst, "rev-parse", "HEAD")+"\n")
			if inputRef := tc.inputs["ref"]; inputRef != "" && inputRef != sha {
				assert.Contains(t, string(outputs), "ref="+inputRef+"\n")
			}
			assert.Equal(t, tc.inputs["sparse-checkout"] != "", gitOutput(dst, "config", "--local", "extensions.worktreeConfig") == "true")
			persisted := tc.inputs["persist-credentials"] != "false"
			for _, repo := range repos {
				assert.Equal(t, tc.inputs["clean"] == "false", fileExists(filepath.Join(repo, "untracked")), repo)
				assert.Len(t, strings.Fields(gitOutput(repo, "config", "--local", "--get-all", credentialsInclude)), 1, repo)
				headers, _ := exec.Command("git", "-C", repo, "config", "--get-all", "http.file://"+server+"/.extraheader").Output()
				assert.Equal(t, persisted, string(headers) == "\nAuthorization: Basic dG9rZW46dG9r\n", repo)
			}
			if tc.inputs["ssh-key"] != "" {
				keys, _ := filepath.Glob(filepath.Join(runnerTemp, "git-ssh-key-*"))
				require.Len(t, keys, 1)
				key, _ := os.ReadFile(keys[0])
				if persisted {
					assert.Equal(t, "KEY\n", string(key))
				} else {
					assert.Empty(t, key)
				}
				args, _ := os.ReadFile(filepath.Join(runnerTemp, "ssh-args"))
				assert.Contains(t, string(args), "StrictHostKeyChecking=yes")
				assert.Equal(t, tc.inputs["ssh-known-hosts"] != "", strings.Contains(string(args), "UserKnownHostsFile="))
				assert.Equal(t, persisted, gitOutput(dst, "config", "core.sshCommand") != "")
			}
		})
	}
}

func publish(t *testing.T, server, name string, files, submodules map[string]string) {
	work := t.TempDir()
	runGit(t, "init", "--initial-branch=main", work)
	for file, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(work, file)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, file), []byte(content), 0o644))
	}
	runGit(t, "-C", work, "add", ".")
	for path, url := range submodules {
		runGit(t, "-C", work, "config", "-f", ".gitmodules", "submodule."+path+".path", path)
		runGit(t, "-C", work, "config", "-f", ".gitmodules", "submodule."+path+".url", url)
		commit := runGit(t, "--git-dir", filepath.Join(server, name, url), "rev-parse", "main")
		runGit(t, "-C", work, "update-index", "--add", "--cacheinfo", "160000,"+commit+","+path)
		runGit(t, "-C", work, "add", ".gitmodules")
	}
	runGit(t, "-C", work, "commit", "-m", "content")
	runGit(t, "clone", "--bare", "--quiet", work, filepath.Join(server, name))
}

func gitOutput(dir string, args ...string) string {
	out, _ := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out))
}

func fileExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}
