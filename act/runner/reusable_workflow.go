// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"

	"gitea.dev/actionslib/pkg/model"
)

func newLocalReusableWorkflowExecutor(rc *RunContext) common.Executor {
	localWorkflow, err := model.ParseReusableWorkflowUses(rc.Run.Job().Uses)
	if err != nil {
		return common.NewErrorExecutor(err)
	}

	if !rc.Config.NoSkipCheckout {
		// resolve the local workflow against the workspace root, not the process
		// working directory, so it is found regardless of where the runner is invoked
		return newReusableWorkflowExecutor(rc, rc.Config.Workdir, localWorkflow.Path)
	}

	uses := fmt.Sprintf("%s/%s@%s", rc.Config.PresetGitHubContext.Repository, localWorkflow.Path, rc.Config.PresetGitHubContext.Sha)

	remoteReusableWorkflow, err := newRemoteReusableWorkflow(rc.Config.GitHubInstance, uses)
	if err != nil {
		return common.NewErrorExecutor(err)
	}

	workflowDir := fmt.Sprintf("%s/%s", rc.ActionCacheDir(), safeFilename(uses))

	// If the repository is private, we need a token to clone it
	token := rc.Config.GetToken()

	return common.NewPipelineExecutor(
		cloneRemoteReusableWorkflow(rc, remoteReusableWorkflow.CloneURL(), remoteReusableWorkflow.Ref, workflowDir, token),
		newReusableWorkflowExecutor(rc, workflowDir, remoteReusableWorkflow.Path),
	)
}

func newRemoteReusableWorkflowExecutor(rc *RunContext) common.Executor {
	remoteReusableWorkflow, err := newRemoteReusableWorkflow(rc.Config.GitHubInstance, rc.Run.Job().Uses)
	if err != nil {
		return common.NewErrorExecutor(err)
	}

	// uses with safe filename makes the target directory look something like this {owner}-{repo}-.github-workflows-{filename}@{ref}
	// instead we will just use {owner}-{repo}@{ref} as our target directory. This should also improve performance when we are using
	// multiple reusable workflows from the same repository and ref since for each workflow we won't have to clone it again
	filename := fmt.Sprintf("%s/%s@%s", remoteReusableWorkflow.Owner, remoteReusableWorkflow.Repo, remoteReusableWorkflow.Ref)
	workflowDir := fmt.Sprintf("%s/%s", rc.ActionCacheDir(), safeFilename(filename))

	token := getGitCloneToken(rc.Config, remoteReusableWorkflow.CloneURL())

	return common.NewPipelineExecutor(
		cloneRemoteReusableWorkflow(rc, remoteReusableWorkflow.CloneURL(), remoteReusableWorkflow.Ref, workflowDir, token),
		newReusableWorkflowExecutor(rc, workflowDir, remoteReusableWorkflow.Path),
	)
}

// cloneRemoteReusableWorkflow always invokes the clone executor — moving refs
// (branches, tags) must be re-resolved each run, matching GitHub Actions.
//
// Callers must not change remoteReusableWorkflow.URL, because:
//  1. Gitea doesn't support specifying GithubContext.ServerURL by the GITHUB_SERVER_URL env
//  2. Gitea has already full URL with rc.Config.GitHubInstance when calling newRemoteReusableWorkflow
//
// remoteReusableWorkflow.URL = rc.getGithubContext(ctx).ServerURL
func cloneRemoteReusableWorkflow(rc *RunContext, cloneURL, ref, targetDirectory, token string) common.Executor {
	return func(ctx context.Context) error {
		interpolatedURL, err := rc.NewExpressionEvaluator(ctx).Interpolate(ctx, cloneURL)
		if err != nil {
			return fmt.Errorf("unable to interpolate the workflow clone URL: %w", err)
		}
		return git.NewGitCloneExecutor(git.NewGitCloneExecutorInput{
			URL:         interpolatedURL,
			Ref:         ref,
			Dir:         targetDirectory,
			Token:       token,
			OfflineMode: rc.Config.ActionOfflineMode,
			Depth:       rc.Config.ActionCloneDepth,
		})(ctx)
	}
}

func newReusableWorkflowExecutor(rc *RunContext, directory, workflow string) common.Executor {
	return func(ctx context.Context) error {
		// Serialize workflow reads with cache updates.
		planner, err := func() (model.WorkflowPlanner, error) {
			defer git.AcquireCloneLock(directory)()
			return model.NewWorkflowPlanner(path.Join(directory, workflow), true)
		}()
		if err != nil {
			return err
		}

		plan, err := planner.PlanEvent("workflow_call")
		if err != nil {
			return err
		}

		runner, err := newReusableWorkflowRunner(rc)
		if err != nil {
			return err
		}

		return common.NewPipelineExecutor( // For Gitea
			runner.NewPlanExecutor(plan),
			setReusedWorkflowCallerResult(rc, runner),
		)(ctx)
	}
}

func newReusableWorkflowRunner(rc *RunContext) (*runnerImpl, error) {
	runner := &runnerImpl{
		config:    rc.Config,
		eventJSON: rc.EventJSON,
		caller: &caller{
			runContext: rc,

			reusedWorkflowJobResults: map[string]string{}, // For Gitea
		},
	}

	return runner.configure()
}

type remoteReusableWorkflow struct {
	*model.ReusableWorkflowUses
	URL string
}

func (r *remoteReusableWorkflow) CloneURL() string {
	// In Gitea, r.URL always has the protocol prefix, we don't need to add extra prefix in this case.
	if strings.HasPrefix(r.URL, "http://") || strings.HasPrefix(r.URL, "https://") {
		return fmt.Sprintf("%s/%s/%s", r.URL, r.Owner, r.Repo)
	}
	return fmt.Sprintf("https://%s/%s/%s", r.URL, r.Owner, r.Repo)
}

var absoluteReusableWorkflowURLRegex = regexp.MustCompile(`^(https?://.*)/([^/]+/[^/]+/\.[^/]+/workflows/[^@]+@.*)$`)

// For Gitea
// newRemoteReusableWorkflow parses a remote `uses`, an absolute URL up to `{owner}/{repo}/.{git_platform}/workflows/` replaces baseURL
func newRemoteReusableWorkflow(baseURL, uses string) (*remoteReusableWorkflow, error) {
	if matches := absoluteReusableWorkflowURLRegex.FindStringSubmatch(uses); matches != nil {
		baseURL, uses = matches[1], matches[2]
	}
	parsed, err := model.ParseReusableWorkflowUses(uses)
	if err != nil {
		return nil, err
	}
	if parsed.IsLocal() {
		return nil, fmt.Errorf("expected a remote reusable workflow, got %q", uses)
	}
	return &remoteReusableWorkflow{ReusableWorkflowUses: parsed, URL: baseURL}, nil
}

// For Gitea
func setReusedWorkflowCallerResult(rc *RunContext, runner *runnerImpl) common.Executor {
	return func(ctx context.Context) error {
		caller := runner.caller

		allJobDone := true
		hasFailure := false
		for _, result := range caller.reusedWorkflowJobResults {
			if result == "pending" {
				allJobDone = false
				break
			}
			if result == "failure" {
				hasFailure = true
			}
		}

		if allJobDone {
			reusedWorkflowJobResult := "success"
			reusedWorkflowJobResultMessage := "succeeded"
			if hasFailure {
				reusedWorkflowJobResult = "failure"
				reusedWorkflowJobResultMessage = "failed"
			}

			if rc.caller != nil {
				rc.caller.setReusedWorkflowJobResult(rc.Run.JobID, reusedWorkflowJobResult)
			} else {
				// Serialize this shared Job.Result write against the other matrix combos
				// and setJobResult (same lockJob key).
				unlock := lockJob(rc.Run.Job())
				rc.result(reusedWorkflowJobResult)
				unlock()
				common.Logger(ctx).WithField("jobResult", reusedWorkflowJobResult).Infof("Job %s", reusedWorkflowJobResultMessage)
			}
		}

		return nil
	}
}

// For Gitea
// getGitCloneToken returns GITEA_TOKEN when shouldCloneURLUseToken returns true,
// otherwise returns an empty string
func getGitCloneToken(conf *Config, cloneURL string) string {
	if !shouldCloneURLUseToken(conf.GitHubInstance, conf.trustedActionInstance(), cloneURL) {
		return ""
	}
	return conf.GetToken()
}

// For Gitea
// trustedActionInstance returns the self-hosted DEFAULT_ACTIONS_URL host that may carry the
// task token, or "" when actions resolve to github.com / a GithubMirror (never trusted).
func (c Config) trustedActionInstance() string {
	if c.DefaultActionInstanceIsSelfHosted {
		return c.DefaultActionInstance
	}
	return ""
}

// For Gitea
// shouldCloneURLUseToken returns true when the following conditions are met:
//  1. cloneURL's host matches this Gitea instance: either the registered instance
//     (instanceURL) or, for DEFAULT_ACTIONS_URL=self on a different hostname, the
//     self-hosted action instance (trustedActionInstance, "" when not trusted)
//  2. the cloneURL does not have basic auth embedded
func shouldCloneURLUseToken(instanceURL, trustedActionInstance, cloneURL string) bool {
	u2, err := url.Parse(cloneURL)
	if err != nil || u2.User != nil {
		return false
	}

	for _, candidate := range []string{instanceURL, trustedActionInstance} {
		if candidate == "" {
			continue
		}
		if !strings.HasPrefix(candidate, "http://") &&
			!strings.HasPrefix(candidate, "https://") {
			candidate = "https://" + candidate
		}
		if u1, err := url.Parse(candidate); err == nil && u1.Host == u2.Host {
			return true
		}
	}
	return false
}
