// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func setFetch(t *testing.T, fetch func(context.Context, git.NewGitCloneExecutorInput) (string, string, error)) {
	original := stepActionRemoteFetch
	stepActionRemoteFetch = fetch
	t.Cleanup(func() { stepActionRemoteFetch = original })
}

func TestShortSHAActionRejected(t *testing.T) {
	actionRoot := t.TempDir()
	repo := filepath.Join(actionRoot, "actions", "hello-world-docker-action")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitMust(t, "", "init", "--initial-branch=main", repo)
	gitMust(t, repo, "config", "user.email", "test@test")
	gitMust(t, repo, "config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "action.yml"),
		[]byte("name: hello\nruns:\n  using: node24\n  main: index.js\n"), 0o644))
	gitMust(t, repo, "add", ".")
	gitMust(t, repo, "commit", "-m", "initial")
	output, err := exec.Command("git", "-C", repo, "rev-parse", "--short=7", "HEAD").Output()
	require.NoError(t, err)

	workflowDir := t.TempDir()
	workflow := fmt.Sprintf("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/hello-world-docker-action@%s\n", strings.TrimSpace(string(output)))
	require.NoError(t, os.WriteFile(filepath.Join(workflowDir, "push.yml"), []byte(workflow), 0o644))

	runner, err := New(&Config{
		Workdir:               workflowDir,
		EventName:             "push",
		GitHubInstance:        "github.com",
		DefaultActionInstance: actionRoot,
		ContainerMaxLifetime:  time.Hour,
		PlatformPicker:        func([]string) string { return baseImage },
	})
	require.NoError(t, err)
	planner, err := model.NewWorkflowPlanner(workflowDir, true)
	require.NoError(t, err)
	plan, err := planner.PlanEvent("push")
	require.NoError(t, err)

	err = runner.NewPlanExecutor(plan)(common.WithDryrun(t.Context(), true))
	require.ErrorContains(t, err, "shortened version of a commit SHA")
}

func TestStepActionRemote(t *testing.T) {
	for _, tt := range []struct {
		name, condition string
		status          model.StepStatus
		runError        error
	}{
		{name: "run-successful", status: model.StepStatusSuccess},
		{name: "run-skipped", condition: "false", status: model.StepStatusSkipped},
		{name: "run-error", status: model.StepStatusFailure, runError: assert.AnError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()

			cm := &containerMock{}
			sarm := &stepActionMocks{}

			clonedAction := false
			actionDir := t.TempDir()
			setFetch(t, func(_ctx context.Context, _input git.NewGitCloneExecutorInput) (string, string, error) {
				clonedAction = true
				return actionDir, "", nil
			})

			sar := &stepActionRemote{
				RunContext: &RunContext{
					Config: &Config{
						GitHubInstance: "github.com",
						ActionCacheDir: t.TempDir(),
					},
					Run: &model.Run{
						JobID: "1",
						Workflow: &model.Workflow{
							Jobs: map[string]*model.Job{
								"1": {},
							},
						},
					},
					StepResults:  map[string]*model.StepResult{},
					JobContainer: cm,
				},
				Step:       &model.Step{ID: "step", Uses: "remote/action@v1", If: yaml.Node{Value: tt.condition}},
				readAction: sarm.readAction,
				runAction:  sarm.runAction,
			}
			sar.RunContext.ExprEval = sar.RunContext.NewExpressionEvaluator(ctx)

			sarm.On("readAction", sar.Step, actionDir, "", mock.Anything, mock.Anything).Return(&model.Action{}, nil)
			if tt.status != model.StepStatusSkipped {
				sarm.On("runAction", sar, actionDir, mustNewRemoteAction(t, sar.Step.Uses)).Return(func(_ctx context.Context) error { return tt.runError })

				expectStepFileCommands(t, cm)
			}

			require.NoError(t, sar.pre()(ctx))
			assert.Equal(t, tt.runError, sar.main()(ctx))
			assert.True(t, clonedAction)
			assert.Equal(t, &model.StepResult{Conclusion: tt.status, Outcome: tt.status, Outputs: map[string]string{}}, sar.RunContext.StepResults["step"])

			sarm.AssertExpectations(t)
			cm.AssertExpectations(t)
		})
	}

	t.Run("refreshes composite inputs without leaking environment", func(t *testing.T) {
		step := &stepActionRemote{
			Step:         &model.Step{Uses: "org/composite@v1", With: map[string]string{"SHARED": "first"}},
			RunContext:   &RunContext{Config: &Config{ActionCacheDir: t.TempDir()}, Run: &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}}, JobContainer: &jobContainerMock{}},
			action:       &model.Action{Inputs: map[string]model.Input{"shared": {}}},
			env:          map[string]string{},
			remoteAction: mustNewRemoteAction(t, "org/composite@v1"),
		}

		for _, value := range []string{"first", "second"} {
			step.env["INPUT_SHARED"] = value
			composite, err := step.getCompositeRunContext(t.Context())
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"shared": value}, composite.actionInputs)
			assert.NotContains(t, composite.Env, "INPUT_SHARED")
		}
	})
}

func TestStepActionRemotePrepare(t *testing.T) {
	var fetched, read []string
	setFetch(t, func(_ctx context.Context, input git.NewGitCloneExecutorInput) (string, string, error) {
		fetched = append(fetched, input.URL+"@"+input.Ref)
		if input.Ref == "missing" {
			return "", "", assert.AnError
		}
		return filepath.Join(input.Dir, "commit"), "sha-" + input.Ref, nil
	})
	newJob := func(instance string) *RunContext {
		return &RunContext{
			Config:       &Config{GitHubInstance: instance, ActionCacheDir: t.TempDir()},
			Run:          &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
			JobContainer: &jobContainerMock{},
		}
	}
	prepare := func(rc *RunContext, uses string) (*stepActionRemote, error) {
		sar := &stepActionRemote{Step: &model.Step{Uses: uses}, RunContext: rc, readAction: func(_ctx context.Context, _step *model.Step, _actionDir, actionPath string, _readFile actionYamlReader, _writeFile fileWriter) (*model.Action, error) {
			read = append(read, actionPath)
			return &model.Action{}, nil
		}}
		return sar, sar.prepareActionExecutor()(t.Context())
	}

	job := newJob("https://github.com")
	restore, err := prepare(job, "org/repo/restore@v1")
	require.NoError(t, err)
	root, err := prepare(job, "org/repo@v1")
	require.NoError(t, err)
	nested, err := prepare(&RunContext{Config: job.Config, Run: job.Run, JobContainer: job.JobContainer, Parent: job}, "org/repo/save@v1")
	require.NoError(t, err)
	other, err := prepare(job, "org/repo@v2")
	require.NoError(t, err)
	_, err = prepare(newJob("gitea.example"), "org/repo@v1")
	require.NoError(t, err)
	for range 2 {
		_, err = prepare(job, "org/repo@missing")
		require.ErrorIs(t, err, assert.AnError)
	}

	assert.Equal(t, []string{"https://github.com/org/repo@v1", "https://github.com/org/repo@v2", "https://gitea.example/org/repo@v1", "https://github.com/org/repo@missing"}, fetched)
	assert.Equal(t, []string{"restore", "", "save", "", ""}, read)
	assert.Equal(t, root.actionDir, restore.actionDir)
	assert.Equal(t, root.actionDir, nested.actionDir)
	assert.NotEqual(t, root.actionDir, other.actionDir)

	reference, sha := restore.actionDownloadInfo()
	assert.Equal(t, []string{"org/repo@v1", "sha-v1"}, []string{reference, sha})
	_, sha = root.actionDownloadInfo()
	assert.Empty(t, sha)

	_, rootContainerDir := getContainerActionPaths(root)
	_, restoreContainerDir := getContainerActionPaths(restore)
	assert.NotEqual(t, path.Dir(rootContainerDir), path.Dir(restoreContainerDir))
	assert.Equal(t, "restore", path.Base(restoreContainerDir))
}

func TestStepActionRemotePreCleanup(t *testing.T) {
	for _, test := range []struct {
		name     string
		preIf    string
		preError error
	}{
		{name: "pre ran and main skipped", preIf: "always()"},
		{name: "pre failed before main", preIf: "always()", preError: assert.AnError},
		{name: "pre skipped and main skipped", preIf: "false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cm := &containerMock{}
			sar := &stepActionRemote{
				Step: &model.Step{ID: "step", Uses: "remote/action@v1", If: yaml.Node{Value: "false"}},
				RunContext: &RunContext{
					Config:       &Config{},
					Run:          &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
					StepResults:  map[string]*model.StepResult{},
					JobContainer: cm,
				},
				action: &model.Action{Runs: model.ActionRuns{
					Using: "node24", Pre: "pre.js", PreIf: test.preIf, Post: "post.js", PostIf: "always()",
				}},
				remoteAction: mustNewRemoteAction(t, "remote/action@v1"),
				actionDir:    t.TempDir(),
			}
			sar.RunContext.ExprEval = sar.RunContext.NewExpressionEvaluator(t.Context())
			if test.preIf != "false" {
				_, actionDir := getContainerActionPaths(sar)
				expectStepFileCommands(t, cm)
				cm.On("CopyDir", actionDir+"/", sar.actionDir+"/", false, true).Return(noopExecutor)
				cm.On("Exec", nodeActionCommand(path.Join(actionDir, "pre.js")), mock.Anything, "", "").Return(func(_ctx context.Context) error { return test.preError }).Once()
				cm.On("Exec", nodeActionCommand(path.Join(actionDir, "post.js")), mock.Anything, "", "").Return(noopExecutor).Once()
			}

			assert.Equal(t, test.preError, sar.pre()(t.Context()))
			if test.preError == nil {
				require.NoError(t, sar.main()(t.Context()))
				assert.Equal(t, model.StepStatusSkipped, sar.RunContext.StepResults["step"].Conclusion)
			}
			require.NoError(t, sar.post()(t.Context()))
			cm.AssertExpectations(t)
		})
	}

	skippedComposite := &stepActionRemote{
		Step:           &model.Step{ID: "step"},
		RunContext:     &RunContext{StepResults: map[string]*model.StepResult{"step": {Conclusion: model.StepStatusSkipped}}},
		action:         &model.Action{Runs: model.ActionRuns{Using: "composite"}},
		preStepStarted: true,
	}
	assert.False(t, shouldRunPostStep(skippedComposite)(t.Context()))
}

func Test_newRemoteAction(t *testing.T) {
	check := func(action string, want *remoteAction, wantCloneURL string) {
		t.Run(action, func(t *testing.T) {
			got, _ := newRemoteAction(action, nil, "gitea.example.com")
			assert.Equalf(t, want, got, "newRemoteAction(%v)", action)
			cloneURL := ""
			if got != nil {
				cloneURL = got.CloneURL("github.com")
			}
			assert.Equalf(t, wantCloneURL, cloneURL, "newRemoteAction(%v).CloneURL()", action)
		})
	}
	for _, instance := range []string{"", "https://gitea.com", "http://gitea.com", "ssh://git@gitea.com"} {
		for _, repository := range []struct{ name, path string }{{"heroku", ""}, {"aws", "ec2"}} {
			action := path.Join("actions", repository.name, repository.path) + "@main"
			if instance != "" {
				action = instance + "/" + action
			}
			check(action, &remoteAction{URL: instance, Org: "actions", Repo: repository.name, Path: repository.path, Ref: "main"},
				cmp.Or(instance, "https://github.com")+"/actions/"+repository.name)
		}
	}
	for _, action := range []string{"./.github/actions/my-action", "docker://alpine:3.8", "ssh://gitea.com/onlyonesegment@main"} {
		check(action, nil, "")
	}
	check("self:owner/repo/sub@v1", &remoteAction{URL: "https://gitea.example.com", Org: "owner", Repo: "repo", Path: "sub", Ref: "v1"},
		"https://gitea.example.com/owner/repo")
	_, err := newRemoteAction("self:owner/repo@v1", nil, "")
	require.ErrorContains(t, err, "without a Gitea instance")
}

func mustNewRemoteAction(t *testing.T, uses string) *remoteAction {
	t.Helper()
	action, err := newRemoteAction(uses, nil, "")
	require.NoError(t, err)
	return action
}

func Test_newRemoteActionSelfRepo(t *testing.T) {
	workflow := &model.GithubContext{
		ServerURL:  "https://gitea.example.com",
		Repository: "owner/workflow-repo",
		Sha:        "abc123",
	}
	composite := &model.GithubContext{
		ServerURL:        "https://gitea.example.com",
		Repository:       "owner/workflow-repo",
		Sha:              "abc123",
		ActionRepository: "other/action-repo",
		ActionRef:        "v1",
	}
	tests := []struct {
		name   string
		action string
		github *model.GithubContext
		want   *remoteAction
	}{
		{
			name:   "top level resolves to the workflow repo at its commit",
			action: "$/.gitea/actions/build",
			github: workflow,
			want: &remoteAction{
				URL:  "https://gitea.example.com",
				Org:  "owner",
				Repo: "workflow-repo",
				Path: ".gitea/actions/build",
				Ref:  "abc123",
			},
		},
		{
			name:   "inside a composite resolves to the enclosing action",
			action: "$/.gitea/actions/build",
			github: composite,
			want: &remoteAction{
				URL:  "https://gitea.example.com",
				Org:  "other",
				Repo: "action-repo",
				Path: ".gitea/actions/build",
				Ref:  "v1",
			},
		},
		{name: "unknown instance", action: "$/.gitea/actions/build", github: &model.GithubContext{Repository: "owner/workflow-repo", Sha: "abc123"}},
		{name: "empty path", action: "$/", github: workflow},
		{name: "ref suffix is not allowed", action: "$/.gitea/actions/build@v1", github: workflow},
		{name: "path traversal", action: "$/../escape", github: workflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := newRemoteAction(tt.action, tt.github, "")
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_stepActionRemoteSelfRepoContainerKeyDiffersPerRepo(t *testing.T) {
	keyFor := func(repo string) string {
		sar := &stepActionRemote{
			Step:         &model.Step{Uses: "$/.gitea/actions/build"},
			remoteAction: &remoteAction{Org: "owner", Repo: repo, Ref: "v1"},
		}
		return sar.containerKey()
	}
	assert.NotEqual(t, keyFor("one"), keyFor("two"))
}

func Test_remoteActionReference(t *testing.T) {
	tests := []struct {
		uses string
		want string
	}{
		{uses: "actions/checkout@v7", want: "actions/checkout@v7"},
		{uses: "actions/aws/ec2@main", want: "actions/aws/ec2@main"},
		{uses: "https://gitea.example.com/actions/checkout@v7", want: "actions/checkout@v7"},
	}
	for _, tt := range tests {
		t.Run(tt.uses, func(t *testing.T) {
			assert.Equal(t, tt.want, mustNewRemoteAction(t, tt.uses).Reference())
		})
	}
}

func TestStepActionRemotePreResolvesDownloadedCommit(t *testing.T) {
	instance := t.TempDir()
	actionDir := filepath.Join(instance, "actions", "setup-go")
	require.NoError(t, os.MkdirAll(actionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(actionDir, "action.yml"),
		[]byte("name: setup-go\nruns:\n  using: node20\n  main: index.js\n"), 0o600))

	for _, args := range [][]string{
		{"init", "--initial-branch=main", actionDir},
		{"-C", actionDir, "add", "action.yml"},
		{"-C", actionDir, "-c", "user.name=runner", "-c", "user.email=runner@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "action"},
	} {
		cmd := exec.Command("git", args...)
		require.NoError(t, cmd.Run(), "git %v", args)
	}
	out, err := exec.Command("git", "-C", actionDir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	wantSha := strings.TrimSpace(string(out))

	sar := &stepActionRemote{
		Step: &model.Step{Uses: "actions/setup-go@main"},
		RunContext: &RunContext{
			Config: &Config{
				GitHubInstance:        "https://gitea.example.com",
				DefaultActionInstance: instance,
				ActionCacheDir:        t.TempDir(),
			},
			Run: &model.Run{
				JobID:    "1",
				Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}},
			},
		},
		readAction: readActionImpl,
	}

	require.NoError(t, printPrepareActions(sar.RunContext, []actionPreparer{sar})(t.Context()))
	require.NoError(t, sar.prepareActionExecutor()(t.Context()))

	reference, sha := sar.actionDownloadInfo()
	assert.Equal(t, "actions/setup-go@main", reference)
	assert.Equal(t, wantSha, sha)
}

func Test_safeFilename(t *testing.T) {
	tests := []struct {
		s    string
		want string
	}{
		{
			s:    "https://test.com/test/",
			want: "https---test.com-test-",
		},
		{
			s:    `<>:"/\|?*`,
			want: "---------",
		},
	}
	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			assert.Equalf(t, tt.want, safeFilename(tt.s), "safeFilename(%v)", tt.s)
		})
	}
}

func TestStepActionRemoteCloneTokenSurvivesNilSecrets(t *testing.T) {
	const wantToken = "job-token"

	table := []struct {
		name                  string
		gitHubInstance        string
		defaultActionInstance string
		wantCloneToken        string
	}{
		{
			name:           "same host forwards token despite nil secrets",
			gitHubInstance: "gitea.example.com",
			wantCloneToken: wantToken,
		},
		{
			name:                  "foreign host is not given the token",
			gitHubInstance:        "gitea.example.com",
			defaultActionInstance: "github.com",
			wantCloneToken:        "",
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()

			var capturedToken string
			actionDir := t.TempDir()
			setFetch(t, func(_ctx context.Context, input git.NewGitCloneExecutorInput) (string, string, error) {
				capturedToken = input.Token
				return actionDir, "", nil
			})

			sarm := &stepActionMocks{}
			sar := &stepActionRemote{
				Step: &model.Step{Uses: "org/repo@v1"},
				RunContext: &RunContext{
					Config: &Config{
						GitHubInstance:        tt.gitHubInstance,
						DefaultActionInstance: tt.defaultActionInstance,
						ActionCacheDir:        t.TempDir(),
						Secrets:               nil,
						Token:                 wantToken,
					},
					Run: &model.Run{
						JobID: "1",
						Workflow: &model.Workflow{
							Jobs: map[string]*model.Job{"1": {}},
						},
					},
					StepResults: map[string]*model.StepResult{},
				},
				readAction: sarm.readAction,
			}
			sar.RunContext.ExprEval = sar.RunContext.NewExpressionEvaluator(ctx)

			sarm.On("readAction", sar.Step, actionDir, "", mock.Anything, mock.Anything).Return(&model.Action{}, nil)

			err := printPrepareActions(sar.RunContext, []actionPreparer{sar})(ctx)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCloneToken, capturedToken)

			sarm.AssertExpectations(t)
		})
	}
}
