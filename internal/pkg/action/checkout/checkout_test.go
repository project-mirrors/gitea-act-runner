// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package checkout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeContainer struct {
	container.ExecutionsEnvironment
	dst, src string
	log      []string
}

func (f *fakeContainer) CopyDir(dst, src string, _, _ bool) common.Executor {
	return func(context.Context) error {
		f.dst, f.src = dst, src
		if out, err := exec.Command("git", "--git-dir", filepath.Join(src, ".git"), "log", "--format=%s").Output(); err == nil {
			f.log = strings.Fields(string(out))
		}
		return nil
	}
}

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

func TestCheckout(t *testing.T) {
	work := t.TempDir()
	runGit(t, "init", "--initial-branch=main", work)
	runGit(t, "-C", work, "commit", "--allow-empty", "-m", "c1")
	sha := runGit(t, "-C", work, "rev-parse", "HEAD")
	runGit(t, "-C", work, "commit", "--allow-empty", "-m", "c2")
	runGit(t, "-C", work, "checkout", "-b", "feature")
	runGit(t, "-C", work, "commit", "--allow-empty", "-m", "f1")
	runGit(t, "-C", work, "checkout", "--orphan", "trunk")
	runGit(t, "-C", work, "commit", "--allow-empty", "-m", "t1")
	runGit(t, "-C", work, "commit", "--allow-empty", "-m", "t2")
	runGit(t, "-C", work, "tag", "trunk", "main")
	server := t.TempDir()
	runGit(t, "clone", "--bare", "--branch", "main", work, filepath.Join(server, "org", "repo"))
	runGit(t, "clone", "--bare", "--branch", "trunk", work, filepath.Join(server, "other", "repo"))
	host := t.TempDir()

	for _, tc := range []struct {
		name          string
		bind, noSkip  bool
		inputs        map[string]string
		dst           string
		log           []string
		hostCopy, err bool
	}{
		{name: "bound workspace is skipped", bind: true},
		{name: "workflow ref copies host workdir", inputs: map[string]string{"ref": "main", "path": "a/../src"}, dst: "/workspace/src", hostCopy: true},
		{name: "bound path clones event sha", bind: true, inputs: map[string]string{"path": "src"}, dst: "/workspace/src", log: []string{"c1"}},
		{name: "no-skip clones event sha", noSkip: true, dst: "/workspace", log: []string{"c1"}},
		{name: "ref clones shallow", inputs: map[string]string{"ref": "refs/heads/feature"}, dst: "/workspace", log: []string{"f1"}},
		{name: "tag ref wins over same-named branch", inputs: map[string]string{"ref": "refs/tags/trunk"}, dst: "/workspace", log: []string{"c2"}},
		{name: "fetch-depth 0 clones full history", inputs: map[string]string{"ref": "feature", "fetch-depth": "0"}, dst: "/workspace", log: []string{"f1", "c2", "c1"}},
		{name: "other repository defaults to its default branch", inputs: map[string]string{"repository": "other/repo"}, dst: "/workspace", log: []string{"t2"}},
		{name: "unsupported input", inputs: map[string]string{"submodules": "true"}, err: true},
		{name: "relative path escaping workspace", inputs: map[string]string{"path": "../x"}, err: true},
		{name: "absolute path", inputs: map[string]string{"path": "/x"}, err: true},
		{name: "negative fetch-depth", inputs: map[string]string{"fetch-depth": "-1"}, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeContainer{}
			err := Main(t.Context(), &action.Context{
				Container:      fc,
				Github:         &model.GithubContext{Repository: "org/repo", Ref: "refs/heads/main", Sha: sha, ServerURL: server},
				Inputs:         tc.inputs,
				HostWorkdir:    host,
				Workspace:      "/workspace",
				BindWorkdir:    tc.bind,
				NoSkipCheckout: tc.noSkip,
			})
			if tc.err {
				require.Error(t, err)
				assert.Empty(t, fc.dst)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.dst, fc.dst)
			assert.Equal(t, tc.log, fc.log)
			assert.Equal(t, tc.hostCopy, strings.HasPrefix(fc.src, host))
			if tc.log != nil {
				assert.NoDirExists(t, fc.src)
			}
		})
	}
}
