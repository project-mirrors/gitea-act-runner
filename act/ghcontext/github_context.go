// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2021 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package ghcontext fills a model.GithubContext from the local git checkout.
// The pure data parts of the context live in the shared
// gitea.dev/actionslib/pkg/model package, only the helpers that need a
// git repository on disk are kept here.
package ghcontext

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"

	"gitea.dev/actionslib/pkg/model"
)

var (
	findGitRef      = git.FindGitRef
	findGitRevision = git.FindGitRevision
	findGithubRepo  = git.FindGithubRepo
)

type pinnedKey struct{}

// WithPinnedCheckout keeps a run's sha and ref fixed when HEAD moves.
func WithPinnedCheckout(ctx context.Context) context.Context {
	if ctx.Value(pinnedKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, pinnedKey{}, &sync.Map{})
}

func pinned(ctx context.Context, key string, lookup func() (string, error)) (string, error) {
	cache, ok := ctx.Value(pinnedKey{}).(*sync.Map)
	if !ok {
		return lookup()
	}
	cached, _ := cache.LoadOrStore(key, sync.OnceValues(lookup))
	lookupOnce, _ := cached.(func() (string, error))
	return lookupOnce()
}

// SetRef resolves the ref of the context from its event payload, falling back
// to the ref checked out in repoPath.
func SetRef(ctx context.Context, ghc *model.GithubContext, repoPath string) {
	logger := common.Logger(ctx)

	// https://docs.github.com/en/actions/learn-github-actions/events-that-trigger-workflows
	// https://docs.github.com/en/developers/webhooks-and-events/webhooks/webhook-events-and-payloads
	switch ghc.EventName {
	case "pull_request_target":
		ghc.Ref = "refs/heads/" + ghc.BaseRef
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		ghc.Ref = fmt.Sprintf("refs/pull/%.0f/merge", ghc.Event["number"])
	case "deployment", "deployment_status":
		ghc.Ref = model.AsString(model.NestedMapLookup(ghc.Event, "deployment", "ref"))
	case "release":
		ghc.Ref = "refs/tags/" + model.AsString(model.NestedMapLookup(ghc.Event, "release", "tag_name"))
	case "push", "create", "workflow_dispatch":
		ghc.Ref = model.AsString(ghc.Event["ref"])
	default:
		defaultBranch := model.AsString(model.NestedMapLookup(ghc.Event, "repository", "default_branch"))
		if defaultBranch != "" {
			ghc.Ref = "refs/heads/" + defaultBranch
		}
	}

	if ghc.Ref == "" {
		ref, err := pinned(ctx, "ref:"+repoPath, func() (string, error) { return findGitRef(ctx, repoPath) })
		if err != nil {
			logger.Warningf("unable to get git ref: %v", err)
		} else {
			logger.Debugf("using github ref: %s", ref)
			ghc.Ref = ref
		}

		repository, exists := ghc.Event["repository"]
		if !exists {
			repository = map[string]any{}
		}
		if repository, ok := repository.(map[string]any); !ok {
			logger.Warn("unable to set default branch to master")
		} else if _, exists := repository["default_branch"]; !exists {
			repository["default_branch"] = "master"
			ghc.Event["repository"] = repository
		}

		if ghc.Ref == "" {
			ghc.Ref = "refs/heads/" + model.AsString(model.NestedMapLookup(ghc.Event, "repository", "default_branch"))
		}
	}
}

// SetSha resolves the commit of the context from its event payload, falling
// back to the revision checked out in repoPath.
func SetSha(ctx context.Context, ghc *model.GithubContext, repoPath string) {
	logger := common.Logger(ctx)

	// https://docs.github.com/en/actions/learn-github-actions/events-that-trigger-workflows
	// https://docs.github.com/en/developers/webhooks-and-events/webhooks/webhook-events-and-payloads
	switch ghc.EventName {
	case "pull_request_target":
		ghc.Sha = model.AsString(model.NestedMapLookup(ghc.Event, "pull_request", "base", "sha"))
	case "deployment", "deployment_status":
		ghc.Sha = model.AsString(model.NestedMapLookup(ghc.Event, "deployment", "sha"))
	case "push", "create", "workflow_dispatch":
		if deleted, ok := ghc.Event["deleted"].(bool); ok && !deleted {
			ghc.Sha = model.AsString(ghc.Event["after"])
		}
	}

	if ghc.Sha == "" {
		sha, err := pinned(ctx, "sha:"+repoPath, func() (string, error) {
			_, revision, err := findGitRevision(ctx, repoPath)
			return revision, err
		})
		if err != nil {
			logger.Warningf("unable to get git revision: %v", err)
		} else {
			ghc.Sha = sha
		}
	}
}

// SetRepositoryAndOwner resolves the repository of the context from the git
// remote in repoPath when it is not set yet, and derives its owner.
func SetRepositoryAndOwner(ctx context.Context, ghc *model.GithubContext, githubInstance, repoPath string) {
	if ghc.Repository == "" {
		repo, err := findGithubRepo(ctx, repoPath, githubInstance)
		if err != nil {
			common.Logger(ctx).Warningf("unable to get git repo (githubInstance: %v, repoPath: %v): %v", githubInstance, repoPath, err)
			return
		}
		ghc.Repository = repo
	}
	ghc.RepositoryOwner = strings.Split(ghc.Repository, "/")[0]
}
