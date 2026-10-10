// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/internal/pkg/lock"

	log "github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindGitSlug(t *testing.T) {
	for _, testcase := range []struct {
		remoteURL string
		slug      string
	}{
		{"https://git-codecommit.us-east-1.amazonaws.com/v1/repos/my-repo-name", "my-repo-name"},
		{"ssh://git-codecommit.us-west-2.amazonaws.com/v1/repos/my-repo", "my-repo"},
		{"git@github.com:nektos/act.git", "nektos/act"},
		{"git@github.com:nektos/act", "nektos/act"},
		{"https://github.com/nektos/act.git", "nektos/act"},
		{"http://github.com/nektos/act.git", "nektos/act"},
		{"https://github.com/nektos/act", "nektos/act"},
		{"http://github.com/nektos/act", "nektos/act"},
		{"git+ssh://git@github.com/owner/repo.git", "owner/repo"},
		{"https://example.com:3000/gitea/owner/repo.git/", "owner/repo"},
		{"ssh://git@example.com:2222/gitea/owner/repo.git", "owner/repo"},
		{"example.com:owner/repo.git", "owner/repo"},
		{"http://myotherrepo.com/act.git", ""},
		{"https://user:secret@example.com/act.git", ""},
		{"/tmp/owner/repo.git", ""},
		{"./owner/repo:branch", ""},
	} {
		t.Run(testcase.remoteURL, func(t *testing.T) {
			slug, err := findGitSlug(testcase.remoteURL)
			assert.Equal(t, testcase.slug == "", err != nil)
			assert.NotContains(t, fmt.Sprint(err), "secret")
			assert.Equal(t, testcase.slug, slug)
		})
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

func TestFindGitMetadataOfSHA256RepositoryUsesOrigin(t *testing.T) {
	t.Parallel()
	basedir := t.TempDir()
	const remoteURL = "https://github.com/owner/repo.git"
	require.NoError(t, gitCmd("init", "--object-format=sha256", "--initial-branch=main", basedir))
	require.NoError(t, cleanGitHooks(basedir))
	require.NoError(t, gitCmd("-C", basedir, "commit", "--allow-empty", "-m", "init"))
	require.NoError(t, gitCmd("-C", basedir, "remote", "add", "origin", remoteURL))
	require.NoError(t, gitCmd("-C", basedir, "config", "--add", "remote.origin.url", "https://github.com/other/repo.git"))

	shortSHA, sha, err := FindGitRevision(context.Background(), basedir)
	require.NoError(t, err)
	require.Equal(t, gitRevParse(t, basedir, "HEAD"), sha)
	require.Len(t, sha, 64)
	require.Equal(t, sha[:7], shortSHA)

	ref, err := FindGitRef(context.Background(), basedir)
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", ref)

	url, err := findGitRemoteURL(context.Background(), basedir)
	require.NoError(t, err)
	require.Equal(t, remoteURL, url)

	slug, err := FindGithubRepo(context.Background(), basedir)
	require.NoError(t, err)
	require.Equal(t, "owner/repo", slug)
}

func TestGitFindRef(t *testing.T) {
	t.Parallel()
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
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "action.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755))
	require.NoError(t, gitCmd("-C", workDir, "add", "action.sh"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "-m", "initial"))
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

	require.NoError(t, gitCmd("-C", workDir, "push", "origin", "main:refs/heads/"+fullSha[:4]))

	hostConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(hostConfig, []byte("[core]\n\tautocrlf = true\n[url \""+remoteDir+"\"]\n\tinsteadOf = https://example.invalid/action\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", hostConfig)
	t.Setenv("GIT_DIR", t.TempDir())

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
		"HEAD": {
			Ref: "HEAD",
		},
		"hex branch named like its own commit": {
			Ref: fullSha[:4],
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			clone := NewGitCloneExecutor(NewGitCloneExecutorInput{
				URL: "https://example.invalid/action",
				Ref: tt.Ref,
				Dir: dir,
			})

			err := clone(context.Background())
			if tt.Err != nil {
				assert.Error(t, err) //nolint:testifylint // pre-existing issue from nektos/act
				assert.Equal(t, tt.Err, err)
				assert.Equal(t, tt.Err, clone(context.Background()), "a retry on the cache keeps the error")
				return
			}
			assert.Empty(t, err) //nolint:testifylint // pre-existing issue from nektos/act
			marker := filepath.Join(dir, "marker")
			require.NoError(t, os.WriteFile(marker, nil, 0o644))
			require.NoError(t, clone(context.Background()))
			assert.FileExists(t, marker, "an insteadOf rewrite must not evict the cache")
			script, err := os.ReadFile(filepath.Join(dir, "action.sh"))
			require.NoError(t, err)
			assert.Equal(t, "#!/bin/sh\necho hi\n", string(script), "host core.autocrlf must not convert action files")
		})
	}
}

func TestGitCloneExecutorReclonesWhenOriginURLChanges(t *testing.T) {
	t.Parallel()
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

	nested := filepath.Join(cacheDir, "nested")
	require.NoError(t, os.Mkdir(nested, 0o755))
	require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{URL: oldRemoteDir, Ref: "main", Dir: nested})(t.Context()))
	assert.DirExists(t, filepath.Join(nested, ".git"), "an empty cache dir inside a clone of the same URL gets its own repository")

	markerPath := filepath.Join(cacheDir, "stale-marker")
	require.NoError(t, os.WriteFile(markerPath, []byte("stale"), 0o644))

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := CloneIfRequired(cancelled, NewGitCloneExecutorInput{URL: oldRemoteDir, Ref: "main", Dir: cacheDir}, log.New())
	require.ErrorIs(t, err, context.Canceled)
	require.FileExists(t, markerPath)

	require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
		URL: newRemoteDir,
		Ref: "main",
		Dir: cacheDir,
	})(t.Context()))

	originURL, err := findGitRemoteURL(t.Context(), cacheDir)
	require.NoError(t, err)
	assert.Equal(t, newRemoteDir, originURL)

	out, err := exec.Command("git", "-C", cacheDir, "log", "--oneline", "-1", "--format=%s").Output()
	require.NoError(t, err)
	assert.Equal(t, "new-action", strings.TrimSpace(string(out)))

	_, err = os.Stat(markerPath)
	require.True(t, os.IsNotExist(err), "stale cached directory should be removed before recloning")
}

func TestGitCloneExecutorFollowsRemoteRefChanges(t *testing.T) {
	t.Parallel()
	remoteDir := t.TempDir()
	require.NoError(t, gitCmd("init", "--bare", "--initial-branch=main", remoteDir))
	workDir := t.TempDir()
	require.NoError(t, gitCmd("clone", remoteDir, workDir))
	require.NoError(t, gitCmd("-C", workDir, "checkout", "-b", "main"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "initial"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "feature-1"))
	require.NoError(t, gitCmd("-C", workDir, "push", "-u", "origin", "main"))

	dir := t.TempDir()
	clone := NewGitCloneExecutor(NewGitCloneExecutorInput{URL: remoteDir, Ref: "main", Dir: dir})
	require.NoError(t, clone(context.Background()))

	require.NoError(t, gitCmd("-C", workDir, "reset", "--hard", "HEAD~1"))
	require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "feature-rewritten"))
	require.NoError(t, gitCmd("-C", workDir, "push", "--force", "origin", "main"))
	require.NoError(t, clone(context.Background()), "a non-fast-forward ref must update")
	assert.Equal(t, "feature-rewritten", gitHeadSubject(t, dir))

	tagClone := NewGitCloneExecutor(NewGitCloneExecutorInput{URL: remoteDir, Ref: "v9", Dir: t.TempDir()})
	require.Error(t, tagClone(context.Background()))
	require.NoError(t, gitCmd("-C", workDir, "tag", "v9"))
	require.NoError(t, gitCmd("-C", workDir, "push", "origin", "v9"))
	require.NoError(t, tagClone(context.Background()), "a tag published after a failed lookup must be fetched")
}

func TestGitCloneExecutorOfflineMode(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

	t.Run("commit hash resolves when the remote accepts a direct shallow fetch", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL: unadvertisedRemote(t, false), Ref: unadvertisedSHA, Dir: dir, Depth: 1,
		})(t.Context()))
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

	t.Run("moving hexadecimal-named branch updates while staying shallow", func(t *testing.T) {
		require.NoError(t, gitCmd("-C", workDir, "push", "origin", "main:deadbeef"))
		dir := t.TempDir()
		clone := NewGitCloneExecutor(NewGitCloneExecutorInput{
			URL: remoteDir, Ref: "deadbeef", Dir: dir, Depth: 1,
		})
		require.NoError(t, clone(t.Context()))
		require.Equal(t, "c3", gitHeadSubject(t, dir))

		require.NoError(t, gitCmd("-C", workDir, "commit", "--allow-empty", "-m", "c4"))
		require.NoError(t, gitCmd("-C", workDir, "push", "origin", "main:deadbeef"))

		require.NoError(t, clone(t.Context()))
		assert.Equal(t, "c4", gitHeadSubject(t, dir), "reused shallow clone should update to the new tip")
		assert.FileExists(t, shallowMarker(dir), "repo should remain shallow after update")
		assert.Equal(t, 1, gitRevCount(t, dir))
	})

	t.Run("sha256 full hash is cloned shallowly, reused without the remote, and its 40-character prefix is short", func(t *testing.T) {
		remote := filepath.Join(t.TempDir(), "remote.git")
		require.NoError(t, gitCmd("init", "--bare", "--object-format=sha256", "--initial-branch=main", remote))
		work := t.TempDir()
		require.NoError(t, gitCmd("init", "--object-format=sha256", "--initial-branch=main", work))
		require.NoError(t, gitCmd("-C", work, "remote", "add", "origin", remote))
		require.NoError(t, gitCmd("-C", work, "commit", "--allow-empty", "-m", "pinned"))
		require.NoError(t, gitCmd("-C", work, "push", "origin", "main"))
		sha := gitRevParse(t, work, "HEAD")
		dir := t.TempDir()
		clone := NewGitCloneExecutor(NewGitCloneExecutorInput{URL: remote, Ref: sha, Dir: dir, Depth: 1})
		require.NoError(t, clone(t.Context()))
		assert.FileExists(t, shallowMarker(dir))
		require.ErrorIs(t, NewGitCloneExecutor(NewGitCloneExecutorInput{URL: remote, Ref: sha[:40], Dir: dir, Depth: 1, OfflineMode: true})(t.Context()), ErrShortRef)
		require.NoError(t, os.RemoveAll(remote))
		require.NoError(t, clone(t.Context()))
		assert.Equal(t, sha, gitRevParse(t, dir, "HEAD"))
	})
}

func TestGitCloneExecutorPinnedHashIgnoresShadowingRef(t *testing.T) {
	t.Parallel()
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
	t.Run("blocks on a lock held by another process despite a trailing separator", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "cache")
		release, err := lock.TryLock(dir)
		require.NoError(t, err)
		defer func() { require.NoError(t, release()) }()
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err := AcquireCloneLock(ctx, dir+string(filepath.Separator))
			require.ErrorIs(t, err, context.DeadlineExceeded)
		})
	})

	t.Run("read-only parent falls back to an in-process lock", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "cache")
		require.NoError(t, os.Mkdir(dir, 0o755))
		require.NoError(t, os.Chmod(parent, 0o555))
		defer func() { require.NoError(t, os.Chmod(parent, 0o755)) }()
		unlock, err := AcquireCloneLock(t.Context(), dir)
		require.NoError(t, err)
		unlock()
	})

	t.Run("same directory serializes", func(t *testing.T) {
		dir := t.TempDir()

		unlock1, err := AcquireCloneLock(t.Context(), dir)
		require.NoError(t, err)

		secondAcquired := make(chan struct{})
		go func() {
			unlock, err := AcquireCloneLock(t.Context(), dir)
			if !assert.NoError(t, err) {
				return
			}
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

		unlockA, err := AcquireCloneLock(t.Context(), dirA)
		require.NoError(t, err)
		defer unlockA()

		done := make(chan struct{})
		go func() {
			unlock, err := AcquireCloneLock(t.Context(), dirB)
			if !assert.NoError(t, err) {
				return
			}
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
func TestNewGitCloneExecutorFetchSendsTokenAndHonoursContext(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		once.Do(func() {
			authorization = request.Header.Get("Authorization")
			close(reached)
		})
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		server.Close()
	})

	dir := filepath.Join(t.TempDir(), "cached-action")
	require.NoError(t, gitCmd("init", dir))
	require.NoError(t, gitCmd("-C", dir, "remote", "add", "origin", server.URL))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- NewGitCloneExecutor(NewGitCloneExecutorInput{URL: server.URL, Ref: "main", Dir: dir, Token: "secret"})(ctx)
	}()

	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the executor never reached the remote")
	}
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("token:secret")), authorization)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("fetch ignored context cancellation")
	}
}
