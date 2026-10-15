// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"

	"gitea.dev/actionslib/pkg/model"
)

type stepActionRemote struct {
	Step                *model.Step
	RunContext          *RunContext
	compositeRunContext *RunContext
	compositeSteps      *compositeSteps
	readAction          readAction
	runAction           runAction
	action              *model.Action
	env                 map[string]string
	remoteAction        *remoteAction
	actionDir           string // the repository at the resolved commit, shared by the job's actions in it
	resolvedSha         string // set on the step that made the job's download of the repository
}

type actionDownload struct {
	dir string
	err error
}

var stepActionRemoteFetch = git.FetchAction

func (sar *stepActionRemote) prepareActionExecutor() common.Executor {
	return func(ctx context.Context) error {
		if sar.remoteAction != nil && sar.action != nil {
			// we are already good to run
			return nil
		}

		// For gitea:
		// Since actions can specify the download source via a url prefix.
		// The prefix may contain some sensitive information that needs to be stored in secrets,
		// so we need to interpolate the expression value for uses first.
		uses, err := sar.RunContext.NewExpressionEvaluator(ctx).Interpolate(ctx, sar.Step.Uses)
		if err != nil {
			return fmt.Errorf("unable to interpolate uses: %w", err)
		}
		sar.Step.Uses = uses

		github := sar.getGithubContext(ctx) // read before remoteAction is set, so `$/` resolves against the enclosing action
		if sar.remoteAction, err = newRemoteAction(sar.Step.Uses, github, sar.RunContext.Config.GitHubInstance); err != nil {
			return err
		}

		if sar.remoteAction.IsCheckout() && isLocalCheckout(github, sar.Step) && !sar.RunContext.Config.NoSkipCheckout {
			common.Logger(ctx).Debugf("Skipping local actions/checkout because workdir was already copied")
			return nil
		}
		cloneURL := sar.remoteAction.CloneURL(sar.RunContext.Config.DefaultActionURL())
		job := sar.RunContext.topLevelRunContext()
		key := cloneURL + "@" + sar.remoteAction.Ref
		download, ok := job.actionDownloads[key]
		if !ok {
			// For Gitea
			// github.Token, unlike Config.GetToken, survives the composite config copy, which nils Config.Secrets.
			download.dir, sar.resolvedSha, download.err = stepActionRemoteFetch(ctx, sar.RunContext.Config.withInstanceAuth(git.NewGitCloneExecutorInput{
				URL:         cloneURL,
				Ref:         sar.remoteAction.Ref,
				Dir:         filepath.Join(sar.RunContext.ActionCacheDir(), model.UsesHash(key)),
				OfflineMode: sar.RunContext.Config.ActionOfflineMode,
				Depth:       sar.RunContext.Config.ActionCloneDepth,
			}, github.Token))
			if job.actionDownloads == nil {
				job.actionDownloads = map[string]actionDownload{}
			}
			job.actionDownloads[key] = download
		}
		var refErr *git.Error
		switch {
		case errors.As(download.err, &refErr) && errors.Is(download.err, git.ErrShortRef):
			return fmt.Errorf("unable to resolve action `%s`, the provided ref `%s` is the shortened version of a commit SHA, which is not supported. Please use the full commit SHA `%s` instead",
				sar.Step.Uses, sar.remoteAction.Ref, refErr.Commit())
		case download.err != nil:
			return download.err
		}
		sar.actionDir = download.dir

		unlock, err := git.AcquireCloneLock(ctx, sar.actionDir)
		if err != nil {
			return err
		}
		defer unlock()

		remoteReader := func(filename string) (io.Reader, io.Closer, error) {
			f, err := os.Open(filepath.Join(sar.actionDir, sar.remoteAction.Path, filename))
			return f, f, err
		}

		actionModel, err := sar.readAction(ctx, sar.Step, sar.actionDir, sar.remoteAction.Path, remoteReader, os.WriteFile)
		sar.action = actionModel
		return err
	}
}

func (sar *stepActionRemote) actionDownloadInfo() (reference, sha string) { // once per job, as actions/runner reports
	if sar.resolvedSha == "" {
		return "", ""
	}
	return fmt.Sprintf("%s/%s@%s", sar.remoteAction.Org, sar.remoteAction.Repo, sar.remoteAction.Ref), sar.resolvedSha
}

func (sar *stepActionRemote) pre() common.Executor {
	sar.env = map[string]string{}

	return common.NewPipelineExecutor(
		sar.prepareActionExecutor(),
		runStepExecutor(sar, stepStagePre, runPreStep(sar)).If(hasPreStep(sar)).If(shouldRunPreStep(sar)))
}

func (sar *stepActionRemote) main() common.Executor {
	return common.NewPipelineExecutor(
		sar.prepareActionExecutor(),
		runStepExecutor(sar, stepStageMain, func(ctx context.Context) error {
			printRunActionHeader(ctx, sar.Step, sar.env, sar.RunContext)
			rawLogger := common.Logger(ctx).With(rawOutputField, true)
			defer rawLogger.Infof("::endgroup::")

			github := sar.getGithubContext(ctx)
			if sar.remoteAction.IsCheckout() && isLocalCheckout(github, sar.Step) && !sar.RunContext.Config.NoSkipCheckout {
				if sar.RunContext.Config.BindWorkdir {
					common.Logger(ctx).Debugf("Skipping local actions/checkout because you bound your workspace")
					return nil
				}
				copyToPath := path.Join(sar.RunContext.JobContainer.ToContainerPath(sar.RunContext.Config.Workdir), sar.Step.With["path"])
				return sar.RunContext.JobContainer.CopyDir(copyToPath, sar.RunContext.Config.Workdir+string(filepath.Separator)+".", sar.RunContext.Config.UseGitIgnore, false)(ctx)
			}

			return sar.runAction(sar, sar.actionDir, sar.remoteAction)(ctx)
		}),
	)
}

func (sar *stepActionRemote) post() common.Executor {
	return runStepExecutor(sar, stepStagePost, runPostStep(sar)).If(hasPostStep(sar)).If(shouldRunPostStep(sar))
}

func (sar *stepActionRemote) containerKey() string {
	return model.UsesHash(sar.remoteAction.URL + "/" + sar.remoteAction.Reference())
}

func (sar *stepActionRemote) getRunContext() *RunContext {
	return sar.RunContext
}

func (sar *stepActionRemote) getGithubContext(ctx context.Context) *model.GithubContext {
	ghc := sar.getRunContext().getGithubContext(ctx)

	// extend github context if we already have an initialized remoteAction
	remoteAction := sar.remoteAction
	if remoteAction != nil {
		ghc.ActionRepository = fmt.Sprintf("%s/%s", remoteAction.Org, remoteAction.Repo)
		ghc.ActionRef = remoteAction.Ref
	}

	return ghc
}

func (sar *stepActionRemote) getStepModel() *model.Step {
	return sar.Step
}

func (sar *stepActionRemote) getEnv() *map[string]string {
	return &sar.env
}

func (sar *stepActionRemote) getIfExpression(ctx context.Context, stage stepStage) string {
	switch stage {
	case stepStagePre:
		github := sar.getGithubContext(ctx)
		if sar.remoteAction.IsCheckout() && isLocalCheckout(github, sar.Step) && !sar.RunContext.Config.NoSkipCheckout {
			// skip local checkout pre step
			return "false"
		}
		return sar.action.Runs.PreIf
	case stepStageMain:
		return sar.Step.If.Value
	case stepStagePost:
		return sar.action.Runs.PostIf
	}
	return ""
}

func (sar *stepActionRemote) getActionModel() *model.Action {
	return sar.action
}

func (sar *stepActionRemote) getCompositeRunContext(ctx context.Context) (*RunContext, error) {
	if sar.compositeRunContext == nil {
		_, containerActionDir := getContainerActionPaths(sar)

		compositeRunContext, err := newCompositeRunContext(ctx, sar.RunContext, sar, containerActionDir)
		if err != nil {
			return nil, err
		}
		sar.compositeRunContext = compositeRunContext
		sar.compositeSteps = compositeRunContext.compositeExecutor(sar.action)
	} else {
		// Re-evaluate environment here. For remote actions the environment
		// need to be re-created for every stage (pre, main, post) as there
		// might be required context changes (inputs/outputs) while the action
		// stages are executed. (e.g. the output of another action is the
		// input for this action during the main stage, but the env
		// was already created during the pre stage)
		env, err := evaluateCompositeInputAndEnv(ctx, sar.RunContext, sar)
		if err != nil {
			return nil, err
		}
		sar.compositeRunContext.setCompositeActionEnv(env)
		sar.compositeRunContext.ExtraPath = sar.RunContext.ExtraPath
	}
	return sar.compositeRunContext, nil
}

func (sar *stepActionRemote) getCompositeSteps() *compositeSteps {
	return sar.compositeSteps
}

type remoteAction struct {
	URL  string
	Org  string
	Repo string
	Path string
	Ref  string
}

func (ra *remoteAction) CloneURL(u string) string {
	if ra.URL == "" {
		// keep an absolute local path as-is (used by tests to resolve actions from a local
		// repo); only bare host names get the https:// scheme prepended
		if !filepath.IsAbs(u) {
			u = withHTTPS(u)
		}
	} else {
		u = ra.URL
	}

	return fmt.Sprintf("%s/%s/%s", u, ra.Org, ra.Repo)
}

// Reference renders the action as {org}/{repo}[/path]@{ref}, omitting the download source, which
// can be interpolated from a secret.
func (ra *remoteAction) Reference() string {
	repo := fmt.Sprintf("%s/%s", ra.Org, ra.Repo)
	if ra.Path != "" {
		repo = fmt.Sprintf("%s/%s", repo, ra.Path)
	}
	return fmt.Sprintf("%s@%s", repo, ra.Ref)
}

func (ra *remoteAction) IsCheckout() bool {
	if ra.Org == "actions" && ra.Repo == "checkout" {
		return true
	}
	return false
}

// newRemoteAction resolves `self:` on instanceURL and `$/` in the enclosing composite action, else the workflow's repo and commit.
func newRemoteAction(action string, github *model.GithubContext, instanceURL string) (*remoteAction, error) {
	uses, err := model.ParseActionUses(action)
	if err != nil {
		return nil, err
	}
	ra := &remoteAction{URL: uses.URL, Org: uses.Owner, Repo: uses.Repo, Path: uses.Path, Ref: uses.Ref}
	switch uses.Kind {
	case model.ActionUsesInstance:
		if instanceURL == "" {
			return nil, fmt.Errorf("unable to resolve %q without a Gitea instance", action)
		}
		ra.URL = instanceURL
		if !strings.Contains(instanceURL, "://") {
			ra.URL = "https://" + instanceURL
		}
	case model.ActionUsesSelfRepo:
		repo, ref := github.ActionRepository, github.ActionRef
		if repo == "" || ref == "" {
			repo, ref = github.Repository, github.Sha
		}
		ra.Org, ra.Repo, _ = strings.Cut(repo, "/")
		ra.URL, ra.Ref = github.ServerURL, ref
		if ra.URL == "" {
			return nil, fmt.Errorf("unable to resolve %q without a Gitea instance", action)
		}
		if ra.Org == "" || ra.Repo == "" || ra.Ref == "" {
			return nil, fmt.Errorf("unable to resolve %q without a repository and commit", action)
		}
	}
	return ra, nil
}

func safeFilename(s string) string {
	return strings.NewReplacer(
		`<`, "-",
		`>`, "-",
		`:`, "-",
		`"`, "-",
		`/`, "-",
		`\`, "-",
		`|`, "-",
		`?`, "-",
		`*`, "-",
	).Replace(s)
}
