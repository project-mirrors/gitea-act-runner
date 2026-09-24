// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package checkout

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/internal/pkg/action"
)

var inputNames = []string{"repository", "ref", "token", "path", "fetch-depth"}

func Main(ctx context.Context, c *action.Context) error {
	for name := range c.Inputs {
		if !slices.Contains(inputNames, name) {
			return fmt.Errorf("checkout: unsupported input %q", name)
		}
	}
	dst, err := workspaceDestination(c.Workspace, c.Inputs["path"])
	if err != nil {
		return err
	}
	depth, err := fetchDepth(c.Inputs["fetch-depth"])
	if err != nil {
		return err
	}

	ghc := c.Github
	repository := cmp.Or(c.Inputs["repository"], ghc.Repository)
	ref := shortBranch(c.Inputs["ref"])
	isWorkflowRepo := repository == ghc.Repository
	local := isWorkflowRepo && slices.Contains([]string{"", shortBranch(ghc.Ref), ghc.Sha}, ref) && !c.NoSkipCheckout
	if local && !c.BindWorkdir {
		return c.Container.CopyDir(dst, c.HostWorkdir+string(filepath.Separator)+".", c.UseGitIgnore, false)(ctx)
	}
	if local && dst == c.Workspace {
		return nil
	}

	if ref == "" {
		ref = "HEAD"
		if isWorkflowRepo {
			ref = ghc.Sha
		}
	}
	dir, err := os.MkdirTemp("", "checkout-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := git.NewGitCloneExecutor(git.NewGitCloneExecutorInput{
		URL:             strings.TrimSuffix(ghc.ServerURL, "/") + "/" + repository,
		Ref:             ref,
		Dir:             dir,
		Token:           cmp.Or(c.Inputs["token"], ghc.Token),
		Depth:           depth,
		InsecureSkipTLS: c.InsecureSkipTLS,
	})(ctx); err != nil {
		return fmt.Errorf("checkout: clone %s@%s: %w", repository, ref, err)
	}
	return c.Container.CopyDir(dst, dir+string(filepath.Separator)+".", false, false)(ctx)
}

func workspaceDestination(workspace, checkoutPath string) (string, error) {
	cleanPath := path.Clean(strings.TrimSpace(checkoutPath))
	if path.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return "", fmt.Errorf("checkout: path %q must be relative to the workspace", checkoutPath)
	}
	return path.Join(workspace, cleanPath), nil
}

func fetchDepth(input string) (int, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 1, nil
	}
	depth, err := strconv.Atoi(input)
	if err != nil || depth < 0 {
		return 0, fmt.Errorf("checkout: fetch-depth %q must be a non-negative integer", input)
	}
	return depth, nil
}

// NewGitCloneExecutor resolves branches by short name, a full tag ref stays exact.
func shortBranch(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}
