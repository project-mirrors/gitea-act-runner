// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gogitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gogitfile "github.com/go-git/go-git/v5/plumbing/transport/file"
	log "github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindGitSlug(t *testing.T) {
	assert := assert.New(t)

	slugTests := []struct {
		url      string // input
		provider string // expected result
		slug     string // expected result
	}{
		{"https://git-codecommit.us-east-1.amazonaws.com/v1/repos/my-repo-name", "CodeCommit", "my-repo-name"},
		{"ssh://git-codecommit.us-west-2.amazonaws.com/v1/repos/my-repo", "CodeCommit", "my-repo"},
		{"git@github.com:nektos/act.git", "GitHub", "nektos/act"},
		{"git@github.com:nektos/act", "GitHub", "nektos/act"},
		{"https://github.com/nektos/act.git", "GitHub", "nektos/act"},
		{"http://github.com/nektos/act.git", "GitHub", "nektos/act"},
		{"https://github.com/nektos/act", "GitHub", "nektos/act"},
		{"http://github.com/nektos/act", "GitHub", "nektos/act"},
		{"git+ssh://git@github.com/owner/repo.git", "GitHub", "owner/repo"},
		{"http://myotherrepo.com/act.git", "", "http://myotherrepo.com/act.git"},
	}

	for _, tt := range slugTests {
		provider, slug := findGitSlug(tt.url, "github.com")
		assert.Equal(tt.provider, provider)
		assert.Equal(tt.slug, slug)
	}
}

func TestErrorWrapsCommitAndCause(t *testing.T) {
	err := &Error{err: ErrShortRef, commit: "abc123"}
	require.Equal(t, ErrShortRef.Error(), err.Error())
	require.ErrorIs(t, err, ErrShortRef)
	require.Equal(t, "abc123", err.Commit())
}

func cleanGitHooks(dir string) error {
	hooksDir := filepath.Join(dir, ".git", "hooks")
	files, err := os.ReadDir(hooksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		relName := filepath.Join(hooksDir, f.Name())
		if err := os.Remove(relName); err != nil {
			return err
		}
	}
	return nil
}

func TestFindGithubRepoUsesOrigin(t *testing.T) {
	basedir := t.TempDir()
	const remoteURL = "https://github.com/owner/repo.git"
	require.NoError(t, gitCmd("init", basedir))
	require.NoError(t, cleanGitHooks(basedir))
	require.NoError(t, gitCmd("-C", basedir, "remote", "add", "origin", remoteURL))

	url, err := findGitRemoteURL(context.Background(), basedir, "origin")
	require.NoError(t, err)
	require.Equal(t, remoteURL, url)

	slug, err := FindGithubRepo(context.Background(), basedir, "github.com")
	require.NoError(t, err)
	require.Equal(t, "owner/repo", slug)
}

func TestGitFindRef(t *testing.T) {
	basedir := t.TempDir()

	for name, tt := range map[string]struct {
		Prepare func(t *testing.T, dir string)
		Assert  func(t *testing.T, ref string, err error)
	}{
		"new_repo": {
			Prepare: func(t *testing.T, dir string) {},
			Assert: func(t *testing.T, ref string, err error) {
				require.Error(t, err)
			},
		},
		"new_repo_with_commit": {
			Prepare: func(t *testing.T, dir string) {
				require.NoError(t, gitCmd("-C", dir, "commit", "--allow-empty", "-m", "msg"))
			},
			Assert: func(t *testing.T, ref string, err error) {
				require.NoError(t, err)
				require.Equal(t, "refs/heads/master", ref)
			},
		},
		"current_head_is_tag": {
			Prepare: func(t *testing.T, dir string) {
				require.NoError(t, gitCmd("-C", dir, "commit", "--allow-empty", "-m", "commit msg"))
				require.NoError(t, gitCmd("-C", dir, "tag", "v1.2.3"))
				require.NoError(t, gitCmd("-C", dir, "checkout", "v1.2.3"))
			},
			Assert: func(t *testing.T, ref string, err error) {
				require.NoError(t, err)
				require.Equal(t, "refs/tags/v1.2.3", ref)
			},
		},
		"current_head_is_same_as_tag": {
			Prepare: func(t *testing.T, dir string) {
				require.NoError(t, gitCmd("-C", dir, "commit", "--allow-empty", "-m", "1.4.2 release"))
				require.NoError(t, gitCmd("-C", dir, "tag", "v1.4.2"))
			},
			Assert: func(t *testing.T, ref string, err error) {
				require.NoError(t, err)
				require.Equal(t, "refs/tags/v1.4.2", ref)
			},
		},
		"current_head_is_not_tag": {
			Prepare: func(t *testing.T, dir string) {
				require.NoError(t, gitCmd("-C", dir, "commit", "--allow-empty", "-m", "msg"))
				require.NoError(t, gitCmd("-C", dir, "tag", "v1.4.2"))
				require.NoError(t, gitCmd("-C", dir, "commit", "--allow-empty", "-m", "msg2"))
			},
			Assert: func(t *testing.T, ref string, err error) {
				require.NoError(t, err)
				require.Equal(t, "refs/heads/master", ref)
			},
		},
		"current_head_is_another_branch": {
			Prepare: func(t *testing.T, dir string) {
				require.NoError(t, gitCmd("-C", dir, "checkout", "-b", "mybranch"))
				require.NoError(t, gitCmd("-C", dir, "commit", "--allow-empty", "-m", "msg"))
			},
			Assert: func(t *testing.T, ref string, err error) {
				require.NoError(t, err)
				require.Equal(t, "refs/heads/mybranch", ref)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(basedir, name)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, gitCmd("-C", dir, "init", "--initial-branch=master"))
			require.NoError(t, cleanGitHooks(dir))
			tt.Prepare(t, dir)
			ref, err := FindGitRef(context.Background(), dir)
			tt.Assert(t, ref, err)
		})
	}
}

func TestGitCloneExecutor(t *testing.T) {
	// Build a local bare "remote" so this runs offline and fast. The cases below mirror
	// the tag/branch/sha/short-sha ref paths the executor handles, formerly exercised by
	// cloning actions/checkout and anchore/scan-action over the network.
	remoteDir := t.TempDir()
	require.NoError(t, gitCmd("init", "--bare", "--initial-branch=main", remoteDir))

	workDir := t.TempDir()
	require.NoError(t, gitCmd("clone", remoteDir, workDir))
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "main"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "initial"))
	require.NoError(t, gitCmd("-C", workDir, "tag", "v2"))
	require.NoError(t, gitCmd("-C", workDir, "push", "-u", "origin", "main"))
	require.NoError(t, gitCmd("-C", workDir, "push", "origin", "v2"))

	// A branch with a dash in the name (mirrors the historical scan-action@act-fails case).
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "act-fails"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "branch-commit"))
	require.NoError(t, gitCmd("-C", workDir, "push", "origin", "act-fails"))

	out, err := exec.Command("git", "-C", workDir, "rev-parse", "main").Output()
	require.NoError(t, err)
	fullSha := strings.TrimSpace(string(out))

	for name, tt := range map[string]struct {
		Err error
		Ref string
	}{
		"tag": {
			Err: nil,
			Ref: "v2",
		},
		"branch": {
			Err: nil,
			Ref: "act-fails",
		},
		"sha": {
			Err: nil,
			Ref: fullSha,
		},
		"short-sha": {
			Err: &Error{ErrShortRef, fullSha},
			Ref: fullSha[:7],
		},
	} {
		t.Run(name, func(t *testing.T) {
			clone := NewGitCloneExecutor(NewGitCloneExecutorInput{
				URL: remoteDir,
				Ref: tt.Ref,
				Dir: t.TempDir(),
			})

			err := clone(context.Background())
			if tt.Err != nil {
				assert.Error(t, err) //nolint:testifylint // pre-existing issue from nektos/act
				assert.Equal(t, tt.Err, err)
			} else {
				assert.Empty(t, err) //nolint:testifylint // pre-existing issue from nektos/act
			}
		})
	}
}

func TestGitCloneExecutorReclonesWhenOriginURLChanges(t *testing.T) {
	createRemote := func(message string) string {
		remoteDir := t.TempDir()
		require.NoError(t, gitCmd("init", "--bare", "--initial-branch=main", remoteDir))

		workDir := t.TempDir()
		require.NoError(t, gitCmd("clone", remoteDir, workDir))
		require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "main"))
		require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", message))
		require.NoError(t, gitCmd("-C", workDir, "push", "-u", "origin", "main"))

		return remoteDir
	}

	oldRemoteDir := createRemote("old-action")
	newRemoteDir := createRemote("new-action")
	cacheDir := t.TempDir()

	require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
		URL: oldRemoteDir,
		Ref: "main",
		Dir: cacheDir,
	})(t.Context()))

	markerPath := filepath.Join(cacheDir, "stale-marker")
	require.NoError(t, os.WriteFile(markerPath, []byte("stale"), 0o644))

	require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
		URL: newRemoteDir,
		Ref: "main",
		Dir: cacheDir,
	})(t.Context()))

	originURL, err := findGitRemoteURL(t.Context(), cacheDir, "origin")
	require.NoError(t, err)
	assert.Equal(t, newRemoteDir, originURL)

	out, err := exec.Command("git", "-C", cacheDir, "log", "--oneline", "-1", "--format=%s").Output()
	require.NoError(t, err)
	assert.Equal(t, "new-action", strings.TrimSpace(string(out)))

	_, err = os.Stat(markerPath)
	require.True(t, os.IsNotExist(err), "stale cached directory should be removed before recloning")
}

func TestGitCloneExecutorNonFastForwardRef(t *testing.T) {
	// Simulate the scenario where a remote ref (e.g. a GitHub PR head ref) changes
	// non-fast-forward between two fetches. Before the fix, the fetch used Force=false,
	// causing go-git to return ErrForceNeeded and short-circuit the checkout.

	// Create a bare "remote" repo with an initial commit on main and a feature branch.
	remoteDir := t.TempDir()
	require.NoError(t, gitCmd("init", "--bare", "--initial-branch=main", remoteDir))

	// We need a working clone to push commits from.
	workDir := t.TempDir()
	require.NoError(t, gitCmd("clone", remoteDir, workDir))
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "main"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "initial"))
	require.NoError(t, gitCmd("-C", workDir, "push", "-u", "origin", "main"))

	// Create a feature branch (simulates refs/pull/N/head).
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "feature"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "feature-1"))
	require.NoError(t, gitCmd("-C", workDir, "push", "origin", "feature"))

	// First clone via the executor — should succeed and cache the repo.
	cloneDir := t.TempDir()
	clone := NewGitCloneExecutor(NewGitCloneExecutorInput{
		URL: remoteDir,
		Ref: "main",
		Dir: cloneDir,
	})
	require.NoError(t, clone(context.Background()))

	// Now force-push the feature branch to a non-fast-forward commit (simulates
	// a PR rebase). This makes refs/heads/feature non-fast-forward.
	require.NoError(t, gitCmd("-C", workDir, "checkout", "main"))
	require.NoError(t, gitCmd("-C", workDir, "branch", "-D", "feature"))
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "feature"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "feature-rewritten"))
	require.NoError(t, gitCmd("-C", workDir, "push", "--force", "origin", "feature"))

	// Also advance main so we can verify the clone picks up the new commit.
	require.NoError(t, gitCmd("-C", workDir, "checkout", "main"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "second"))
	require.NoError(t, gitCmd("-C", workDir, "push", "origin", "main"))

	// Second clone to the same directory — before the fix this returned ErrForceNeeded
	// and left the working tree at the old commit.
	err := clone(context.Background())
	require.NoError(t, err, "fetch with non-fast-forward refs must not fail when Force=true")

	// Verify the working tree was actually updated to the latest main commit.
	out, err := exec.Command("git", "-C", cloneDir, "log", "--oneline", "-1", "--format=%s").Output()
	require.NoError(t, err)
	assert.Equal(t, "second", strings.TrimSpace(string(out)), "working tree should be at the latest commit")
}

func TestGitCloneExecutorOfflineMode(t *testing.T) {
	// Build a local "remote" with a single commit on main.
	remoteDir := t.TempDir()
	require.NoError(t, gitCmd("init", "--bare", "--initial-branch=main", remoteDir))
	workDir := t.TempDir()
	require.NoError(t, gitCmd("clone", remoteDir, workDir))
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "main"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "initial"))
	require.NoError(t, gitCmd("-C", workDir, "push", "-u", "origin", "main"))

	// Prime the cache with an online clone of main.
	cacheDir := t.TempDir()
	require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
		URL: remoteDir,
		Ref: "main",
		Dir: cacheDir,
	})(context.Background()))

	t.Run("cached branch resolves without fetching", func(t *testing.T) {
		// Offline reuse of a cached branch must succeed even though ResolveRevision(input.Ref)
		// finds no local refs/heads/<ref>.
		err := NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL:         remoteDir,
			Ref:         "main",
			Dir:         cacheDir,
			OfflineMode: true,
		})(context.Background())
		require.NoError(t, err)

		out, err := exec.Command("git", "-C", cacheDir, "log", "--oneline", "-1", "--format=%s").Output()
		require.NoError(t, err)
		assert.Equal(t, "initial", strings.TrimSpace(string(out)))
	})

	t.Run("unresolvable cached ref returns error", func(t *testing.T) {
		// The ref was never cached; offline mode cannot resolve it and must return an error.
		err := NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL:         remoteDir,
			Ref:         "never-fetched",
			Dir:         cacheDir,
			OfflineMode: true,
		})(context.Background())
		require.Error(t, err)
	})
}

func TestGitCloneExecutorQuietDemotesCloneLine(t *testing.T) {
	remoteDir := t.TempDir()
	require.NoError(t, gitCmd("init", "--bare", "--initial-branch=main", remoteDir))
	workDir := t.TempDir()
	require.NoError(t, gitCmd("clone", remoteDir, workDir))
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "main"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "initial"))
	require.NoError(t, gitCmd("-C", workDir, "push", "-u", "origin", "main"))

	// Quiet callers report the download themselves, so the clone line must not reach the job log.
	for name, quiet := range map[string]bool{"quiet": true, "not quiet": false} {
		t.Run(name, func(t *testing.T) {
			logger, hook := logrustest.NewNullLogger()
			logger.SetLevel(log.InfoLevel)
			ctx := common.WithLogger(context.Background(), logger.WithField("job", "j1"))

			require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
				URL:   remoteDir,
				Ref:   "main",
				Dir:   t.TempDir(),
				Quiet: quiet,
			})(ctx))

			var cloneLines int
			for _, entry := range hook.AllEntries() {
				if strings.HasPrefix(entry.Message, "git clone ") {
					cloneLines++
				}
			}
			if quiet {
				assert.Zero(t, cloneLines)
			} else {
				assert.Equal(t, 1, cloneLines)
			}
		})
	}
}

func TestGitCloneExecutorShallow(t *testing.T) {
	// Build a local "remote" with several commits on main plus a tag, so a full clone would pull noticeably more history than a shallow one.
	remoteDir := t.TempDir()
	require.NoError(t, gitCmd("init", "--bare", "--initial-branch=main", remoteDir))
	workDir := t.TempDir()
	require.NoError(t, gitCmd("clone", remoteDir, workDir))
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "main"))
	for _, m := range []string{"c1", "c2", "c3"} {
		require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", m))
	}
	require.NoError(t, gitCmd("-C", workDir, "tag", "v1"))
	unadvertisedSHA := gitRevParse(t, workDir, "HEAD~1")
	require.NoError(t, gitCmd("-C", workDir, "push", "-u", "origin", "main"))
	require.NoError(t, gitCmd("-C", workDir, "push", "origin", "v1"))

	shallowMarker := func(dir string) string { return filepath.Join(dir, ".git", "shallow") }

	t.Run("branch is cloned shallowly", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL: remoteDir, Ref: "main", Dir: dir, Depth: 1,
		})(t.Context()))
		assert.FileExists(t, shallowMarker(dir), "clone should be shallow")
		assert.Equal(t, 1, gitRevCount(t, dir), "only the tip commit should be present")
		assert.Equal(t, "c3", gitHeadSubject(t, dir))
	})

	t.Run("tag is cloned shallowly", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL: remoteDir, Ref: "v1", Dir: dir, Depth: 1,
		})(t.Context()))
		assert.FileExists(t, shallowMarker(dir), "clone should be shallow")
		assert.Equal(t, 1, gitRevCount(t, dir))
		assert.Equal(t, "c3", gitHeadSubject(t, dir))
	})

	unadvertisedRemote := func(t *testing.T, allowUnadvertised bool) string {
		remote := filepath.Join(t.TempDir(), "remote.git")
		require.NoError(t, gitCmd("clone", "--bare", remoteDir, remote))
		require.NoError(t, gitCmd("-C", remote, "config", "uploadpack.allowAnySHA1InWant", strconv.FormatBool(allowUnadvertised)))
		return remote
	}

	t.Run("commit hash falls back to a full clone when the remote refuses unadvertised objects", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL: unadvertisedRemote(t, false), Ref: unadvertisedSHA, Dir: dir, Depth: 1,
		})(t.Context()))
		assert.NoFileExists(t, shallowMarker(dir))
		assert.Equal(t, unadvertisedSHA, gitRevParse(t, dir, "HEAD"))
	})

	t.Run("commit hash is fetched shallowly, moved to another hash, then reused without reaching the remote", func(t *testing.T) {
		remote := unadvertisedRemote(t, true)
		dir := t.TempDir()
		cloneAt := func(ref string) error {
			return NewGitCloneExecutor(NewGitCloneExecutorInput{
				URL: remote, Ref: ref, Dir: dir, Depth: 1,
			})(t.Context())
		}

		require.NoError(t, cloneAt(unadvertisedSHA))
		assert.FileExists(t, shallowMarker(dir))
		assert.Equal(t, 1, gitRevCount(t, dir))
		assert.Equal(t, unadvertisedSHA, gitRevParse(t, dir, "HEAD"))

		olderSHA := gitRevParse(t, workDir, "HEAD~2")
		require.NoError(t, cloneAt(olderSHA))
		assert.Equal(t, olderSHA, gitRevParse(t, dir, "HEAD"))

		require.NoError(t, os.RemoveAll(remote))
		require.NoError(t, cloneAt(olderSHA))
		assert.Equal(t, olderSHA, gitRevParse(t, dir, "HEAD"))
	})

	t.Run("moving branch updates while staying shallow", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL: remoteDir, Ref: "main", Dir: dir, Depth: 1,
		})(t.Context()))
		require.Equal(t, "c3", gitHeadSubject(t, dir))

		// Advance main on the remote, then reuse the existing shallow clone.
		require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "c4"))
		require.NoError(t, gitCmd("-C", workDir, "push", "origin", "main"))

		require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL: remoteDir, Ref: "main", Dir: dir, Depth: 1,
		})(t.Context()))
		assert.Equal(t, "c4", gitHeadSubject(t, dir), "reused shallow clone should update to the new tip")
		assert.FileExists(t, shallowMarker(dir), "repo should remain shallow after update")
		assert.Equal(t, 1, gitRevCount(t, dir))
	})
}

func TestGitCloneExecutorTransportSessions(t *testing.T) {
	workDir := t.TempDir()
	require.NoError(t, gitCmd("init", "--initial-branch=main", workDir))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "c1"))
	require.NoError(t, gitCmd("-C", workDir, "tag", "v1"))
	require.NoError(t, gitCmd("-C", workDir, "tag", "-a", "v2", "-m", "v2"))

	for name, tt := range map[string]struct {
		Ref   string
		Depth int
	}{
		"shallow branch":       {"main", 1},
		"full clone branch":    {"main", 0},
		"full lightweight tag": {"v1", 0},
		"full annotated tag":   {"v2", 0},
	} {
		t.Run(name, func(t *testing.T) {
			counter := installCountingTransport(t)
			dir := t.TempDir()
			clone := NewGitCloneExecutor(NewGitCloneExecutorInput{
				URL: workDir, Ref: tt.Ref, Dir: dir, Depth: tt.Depth,
			})
			require.NoError(t, clone(t.Context()))
			assert.Equal(t, int64(1), counter.sessions.Swap(0), "cold clone")
			assert.Equal(t, gitRevParse(t, workDir, tt.Ref+"^{commit}"), gitRevParse(t, dir, "HEAD"))

			require.NoError(t, clone(t.Context()))
			assert.Equal(t, int64(1), counter.sessions.Swap(0), "unchanged warm cache")

			require.NoError(t, os.WriteFile(filepath.Join(workDir, "action.yml"), []byte(name), 0o644))
			require.NoError(t, gitCmd("-C", workDir, "add", "action.yml"))
			require.NoError(t, gitCmd("-C", workDir, "commit", "-m", name))
			require.NoError(t, gitCmd("-C", workDir, "tag", "--force", "v1"))
			require.NoError(t, gitCmd("-C", workDir, "tag", "--force", "-a", "v2", "-m", "v2"))

			require.NoError(t, os.WriteFile(filepath.Join(dir, "action.yml"), []byte("staged"), 0o644))
			require.NoError(t, gitCmd("-C", dir, "add", "action.yml"))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "action.yml"), []byte("unstaged"), 0o644))

			require.NoError(t, clone(t.Context()))
			assert.Equal(t, int64(1), counter.sessions.Load(), "updated warm cache")
			assert.Equal(t, gitRevParse(t, workDir, "HEAD"), gitRevParse(t, dir, "HEAD"))
			status, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
			require.NoError(t, err)
			assert.Empty(t, string(status))
		})
	}
}

func TestGitCloneExecutorPinnedHashIgnoresShadowingRef(t *testing.T) {
	remoteDir := t.TempDir()
	require.NoError(t, gitCmd("init", "--bare", "--initial-branch=main", remoteDir))
	workDir := t.TempDir()
	require.NoError(t, gitCmd("clone", remoteDir, workDir))
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "main"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "pinned"))
	pinned := gitRevParse(t, workDir, "HEAD")
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "decoy"))
	require.NoError(t, gitCmd("-C", workDir, "tag", pinned))
	require.NoError(t, gitCmd("-C", workDir, "push", "-u", "origin", "main"))
	require.NoError(t, gitCmd("-C", workDir, "push", "origin", pinned))

	for name, depth := range map[string]int{"shallow": 1, "full clone": 0} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
				URL: remoteDir, Ref: pinned, Dir: dir, Depth: depth,
			})(t.Context()))
			assert.Equal(t, pinned, gitRevParse(t, dir, "HEAD"))
		})
	}
}

type countingTransport struct {
	transport.Transport
	sessions atomic.Int64
}

func (c *countingTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	c.sessions.Add(1)
	return c.Transport.NewUploadPackSession(ep, auth)
}

func installCountingTransport(t *testing.T) *countingTransport {
	t.Helper()
	counter := &countingTransport{Transport: gogitfile.DefaultClient}
	gogitclient.InstallProtocol("file", counter)
	t.Cleanup(func() { gogitclient.InstallProtocol("file", gogitfile.DefaultClient) })

	return counter
}

func gitRevParse(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", rev).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func gitRevCount(t *testing.T, dir string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").Output()
	require.NoError(t, err)
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	return n
}

func gitHeadSubject(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func gitCmd(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Inject a deterministic identity and ignore the host's global/system config so commits
	// succeed regardless of the host having no user.name/user.email (e.g. CI, GITHUB_ACTIONS
	// unset) or a global commit.gpgsign, and without mutating the developer's ~/.gitconfig.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Unit Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Unit Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)

	err := cmd.Run()
	if exitError, ok := err.(*exec.ExitError); ok {
		if waitStatus, ok := exitError.Sys().(syscall.WaitStatus); ok {
			return fmt.Errorf("Exit error %d", waitStatus.ExitStatus())
		}
		return exitError
	}
	return nil
}

func TestAcquireCloneLock(t *testing.T) {
	t.Run("same directory serializes", func(t *testing.T) {
		dir := t.TempDir()

		unlock1 := AcquireCloneLock(dir)

		secondAcquired := make(chan struct{})
		go func() {
			unlock := AcquireCloneLock(dir)
			close(secondAcquired)
			unlock()
		}()

		select {
		case <-secondAcquired:
			t.Fatal("second acquire should block while first holds the lock")
		case <-time.After(50 * time.Millisecond):
		}

		unlock1()

		select {
		case <-secondAcquired:
		case <-time.After(time.Second):
			t.Fatal("second acquire should proceed after first releases the lock")
		}
	})

	t.Run("different directories do not block", func(t *testing.T) {
		dirA := t.TempDir()
		dirB := t.TempDir()

		unlockA := AcquireCloneLock(dirA)
		defer unlockA()

		done := make(chan struct{})
		go func() {
			unlock := AcquireCloneLock(dirB)
			unlock()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("acquire on a different directory must not block")
		}
	})
}

// An unresponsive remote must not pin a job: the refresh has to be interruptible.
func TestNewGitCloneExecutorFetchHonoursContext(t *testing.T) {
	block := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(reached) })
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		server.Close()
	})

	dir := filepath.Join(t.TempDir(), "cached-action")
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	_, err = repo.CreateRemote(&gogitconfig.RemoteConfig{Name: "origin", URLs: []string{server.URL}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- NewGitCloneExecutor(NewGitCloneExecutorInput{URL: server.URL, Ref: "main", Dir: dir})(ctx)
	}()

	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the executor never reached the remote")
	}
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("fetch ignored context cancellation")
	}
}
