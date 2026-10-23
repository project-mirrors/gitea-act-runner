// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type closerFunc func()

func (close closerFunc) Close() error {
	close()
	return nil
}

func TestActionReader(t *testing.T) {
	yaml := strings.ReplaceAll(`
name: 'name'
runs:
	using: 'node16'
	main: 'main.js'
`, "\t", "  ")
	yamlAction := &model.Action{
		Name: "name",
		Runs: model.ActionRuns{
			Using:  "node16",
			Main:   "main.js",
			PreIf:  "always()",
			PostIf: "always()",
		},
	}

	table := []struct {
		name        string
		step        *model.Step
		filename    string
		fileContent string
		expected    *model.Action
	}{
		{
			name:        "readActionYml",
			step:        &model.Step{},
			filename:    "action.yml",
			fileContent: yaml,
			expected:    yamlAction,
		},
		{
			name:        "readActionYaml",
			step:        &model.Step{},
			filename:    "action.yaml",
			fileContent: yaml,
			expected:    yamlAction,
		},
		{
			name:        "readDockerfile",
			step:        &model.Step{},
			filename:    "Dockerfile",
			fileContent: "FROM ubuntu:20.04",
			expected: &model.Action{
				Name: "(Synthetic)",
				Runs: model.ActionRuns{
					Using: "docker",
					Image: "Dockerfile",
				},
			},
		},
		{
			name:        "readLowercaseDockerfile",
			step:        &model.Step{},
			filename:    "dockerfile",
			fileContent: "FROM ubuntu:20.04",
			expected: &model.Action{
				Name: "(Synthetic)",
				Runs: model.ActionRuns{
					Using: "docker",
					Image: "dockerfile",
				},
			},
		},
		{
			name: "readWithArgs",
			step: &model.Step{
				With: map[string]string{
					"args": "cmd",
				},
			},
			expected: &model.Action{
				Name: "(Synthetic)",
				Inputs: map[string]model.Input{
					"args": {},
					"cwd": {
						Description: "(Actual working directory)",
						Required:    false,
						Default:     "actionDir/actionPath",
					},
					"command": {
						Description: "(Actual program)",
						Required:    false,
						Default:     "cmd",
					},
				},
				Runs: model.ActionRuns{
					Using: "node12",
					Main:  "trampoline.js",
				},
			},
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			closed := false

			readFile := func(filename string) (io.Reader, io.Closer, error) {
				if tt.filename != filename {
					return nil, nil, fs.ErrNotExist
				}

				return strings.NewReader(tt.fileContent), closerFunc(func() { closed = true }), nil
			}

			writeFile := func(filename string, _data []byte, perm fs.FileMode) error {
				assert.Equal(t, "actionDir/actionPath/trampoline.js", filename)
				assert.Equal(t, fs.FileMode(0o644), perm)
				return nil
			}

			action, err := readActionImpl(t.Context(), tt.step, "actionDir", "actionPath", readFile, writeFile)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, action)

			assert.Equal(t, tt.filename != "", closed)
		})
	}
}

func TestActionRunner(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tt := range []struct {
		name        string
		inputs      map[string]model.Input
		with        map[string]string
		state       map[string]string
		expectedEnv map[string]string
		warning     string
	}{
		{
			name:        "with-input",
			inputs:      map[string]model.Input{"key": {Default: "default value"}},
			expectedEnv: map[string]string{"INPUT_KEY": "default value"},
		},
		{
			name:        "declared input ignores case",
			inputs:      map[string]model.Input{"key": {Default: "default value"}},
			with:        map[string]string{"KEY": "provided value"},
			expectedEnv: map[string]string{"INPUT_KEY": "provided value"},
		},
		{
			name:    "unexpected inputs warn once across stages",
			inputs:  map[string]model.Input{"key": {}},
			with:    map[string]string{"KEY": "provided value", "UNKNOWN": "value", "other%0A\r\n": "value"},
			warning: "##[warning]Unexpected input(s) 'UNKNOWN', 'other%250A%0D%0A', valid inputs are ['key']",
		},
		{
			name:    "no declared inputs",
			with:    map[string]string{"unknown": "value"},
			warning: "##[warning]Unexpected input(s) 'unknown', valid inputs are ['']",
		},
		{
			name:        "restore-saved-state",
			state:       map[string]string{"name": "state value"},
			expectedEnv: map[string]string{"STATE_name": "state value"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logger, hook := logtest.NewLogger()
			ctx := common.WithLogger(t.Context(), logger)
			cm := &containerMock{}
			containerActionDir := "/var/run/act/actions/" + model.UsesHash("/org/repo/path@ref") + "/"
			cm.On("CopyDir", containerActionDir, "dir/", false, true).Return(func(_ctx context.Context) error { return nil })

			envMatcher := mock.MatchedBy(func(env map[string]string) bool {
				for k, v := range tt.expectedEnv {
					if env[k] != v {
						return false
					}
				}
				return true
			})
			cm.On("Exec", []string{"node", "--preserve-symlinks-main", containerActionDir + "path"}, envMatcher, "", "").Return(func(_ctx context.Context) error { return nil })
			cm.On("Exec", nodeActionCommand(containerActionDir+"path/pre.js"), mock.Anything, "", "").Return(func(_ctx context.Context) error { return nil })
			cm.On("Exec", nodeActionCommand(containerActionDir+"path/post.js"), envMatcher, "", "").Return(func(_ctx context.Context) error { return nil })

			remoteAction := mustNewRemoteAction(t, "org/repo/path@ref")
			step := &stepActionRemote{
				Step: &model.Step{ID: "step", Uses: "org/repo/path@ref", With: tt.with},
				RunContext: &RunContext{
					Config:           &Config{PresetGitHubContext: &model.GithubContext{}},
					Run:              &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
					IntraActionState: map[string]map[string]string{"step": tt.state},
					JobContainer:     cm,
				},
				action:       &model.Action{Inputs: tt.inputs, Runs: model.ActionRuns{Using: "node16", Pre: "pre.js", Post: "post.js"}},
				actionDir:    "dir",
				env:          (&model.Step{With: tt.with}).GetEnv(),
				remoteAction: remoteAction,
			}
			require.NoError(t, runPreStep(step)(ctx))
			assert.Empty(t, hook.AllRecords())
			require.NoError(t, runActionImpl(step, "dir", remoteAction)(ctx))
			require.NoError(t, runPostStep(step)(ctx))
			if tt.warning == "" {
				assert.Empty(t, hook.AllRecords())
			} else {
				require.Len(t, hook.AllRecords(), 1)
				assert.Equal(t, tt.warning, hook.LastRecord().Message)
				rawOutput, ok := hook.LastRecord().Attr(rawOutputField)
				require.True(t, ok)
				assert.Equal(t, true, rawOutput)
			}
			cm.AssertExpectations(t)
		})
	}
}

func TestNodeActionCommandPreservesSymlinkedEntrypoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	requireHostTools(t, "node")

	actionDir := t.TempDir()
	entrypoint := filepath.Join(actionDir, "index.mjs")
	require.NoError(t, os.WriteFile(entrypoint, []byte(`
import {fileURLToPath} from "node:url";
const self = fileURLToPath(import.meta.url);
if (process.argv[1] !== self) {
	console.log("argv[1]:", process.argv[1], "import.meta.url:", self);
	process.exitCode = 1;
}
`), 0o600))

	symlinkedActionDir := filepath.Join(t.TempDir(), "action")
	require.NoError(t, os.Symlink(actionDir, symlinkedActionDir))
	symlinkedEntrypoint := filepath.Join(symlinkedActionDir, "index.mjs")

	args := nodeActionCommand(symlinkedEntrypoint)
	output, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestNewStepContainerDoesNotUseDockerSecrets(t *testing.T) {
	cm := &containerMock{}

	var captured *container.NewContainerInput
	origContainerNewContainer := ContainerNewContainer
	ContainerNewContainer = func(input *container.NewContainerInput) container.ExecutionsEnvironment {
		captured = input
		return cm
	}
	defer func() {
		ContainerNewContainer = origContainerNewContainer
	}()

	ctx := t.Context()
	rc := &RunContext{
		Name: "job",
		Config: &Config{
			Secrets: map[string]string{
				"DOCKER_USERNAME": "docker-user",
				"DOCKER_PASSWORD": "docker-password",
			},
		},
		Run: &model.Run{
			JobID: "job",
			Workflow: &model.Workflow{
				Name: "test",
				Jobs: map[string]*model.Job{
					"job": {},
				},
			},
		},
		JobContainer: cm,
		StepResults:  map[string]*model.StepResult{},
	}
	env := map[string]string{}
	step := &stepMock{}
	step.On("getRunContext").Return(rc)
	step.On("getStepModel").Return(&model.Step{ID: "action"})
	step.On("getEnv").Return(&env)

	newStepContainer(ctx, step, "registry.example.com/action:tag", nil, nil, "")

	assert.Empty(t, captured.Username)
	assert.Empty(t, captured.Password)
	assert.True(t, captured.AutoRemove)
	step.AssertExpectations(t)
}

func TestMaybeCopyToActionDirHoldsCloneLock(t *testing.T) {
	ctx := t.Context()

	actionDir := t.TempDir()

	releaseCopy := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseCopy) })
	defer release()

	copyEntered := make(chan struct{})

	cm := &containerMock{}
	cm.On("CopyDir", "/var/run/act/actions/", actionDir+"/", false, true).Return(func(ctx context.Context) error {
		close(copyEntered)
		<-releaseCopy
		return nil
	})

	step := &stepActionRemote{
		Step: &model.Step{Uses: "remote/action@v1"},
		RunContext: &RunContext{
			Config:       &Config{},
			JobContainer: cm,
		},
	}

	copyDone := make(chan error, 1)
	go func() {
		copyDone <- maybeCopyToActionDir(ctx, step, actionDir, "", "/var/run/act/actions/")
	}()

	select {
	case <-copyEntered:
	case err := <-copyDone:
		t.Fatalf("maybeCopyToActionDir returned before CopyDir was entered: %v", err)
	case <-time.After(time.Second):
		t.Fatal("CopyDir was not entered within 1 second")
	}

	peerAcquired := make(chan struct{})
	go func() {
		unlock, err := git.AcquireCloneLock(t.Context(), actionDir)
		if !assert.NoError(t, err) {
			return
		}
		close(peerAcquired)
		unlock()
	}()

	select {
	case <-peerAcquired:
		t.Fatal("peer AcquireCloneLock returned while CopyDir was running")
	case <-time.After(50 * time.Millisecond):
	}

	release()

	select {
	case err := <-copyDone:
		if err != nil {
			t.Fatalf("maybeCopyToActionDir returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("maybeCopyToActionDir did not return after CopyDir was unblocked")
	}

	select {
	case <-peerAcquired:
	case <-time.After(time.Second):
		t.Fatal("peer AcquireCloneLock did not proceed after lock released")
	}

	cm.AssertExpectations(t)
}

func TestExecAsDockerHoldsCloneLockForRemoteUncached(t *testing.T) {
	actionDir := t.TempDir()

	unlock, err := git.AcquireCloneLock(t.Context(), actionDir)
	require.NoError(t, err)
	unlockOnce := sync.OnceFunc(unlock)
	defer unlockOnce()

	innerEntered := make(chan struct{})
	releaseInner := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(releaseInner) })
	defer releaseOnce()

	origImageExists := ContainerImageExistsLocally
	ContainerImageExistsLocally = func(_ context.Context, _, _ string) (bool, error) {
		return false, nil
	}
	defer func() { ContainerImageExistsLocally = origImageExists }()

	origBuildExec := ContainerNewDockerBuildExecutor
	ContainerNewDockerBuildExecutor = func(_ container.NewDockerBuildExecutorInput) common.Executor {
		return func(_ context.Context) error {
			close(innerEntered)
			<-releaseInner
			return nil
		}
	}
	defer func() { ContainerNewDockerBuildExecutor = origBuildExec }()

	step := &stepActionRemote{
		Step: &model.Step{ID: "1", Uses: "remote/action@v1", With: map[string]string{}},
		RunContext: &RunContext{
			Config: &Config{},
			Run: &model.Run{
				JobID: "1",
				Workflow: &model.Workflow{
					Name: "wf",
					Jobs: map[string]*model.Job{"1": {}},
				},
			},
			JobContainer: &containerMock{},
		},
		action: &model.Action{Runs: model.ActionRuns{Using: "docker", Image: "Dockerfile"}},
		env:    map[string]string{},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- execAsDocker(ctx, step, "test-action", actionDir, actionDir, false, stepStageMain) }()

	select {
	case <-innerEntered:
		t.Fatal("inner build executor ran before clone lock was released")
	case err := <-done:
		t.Fatalf("execAsDocker returned before inner was entered: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	unlockOnce()

	select {
	case <-innerEntered:
	case err := <-done:
		t.Fatalf("execAsDocker returned without entering inner: %v", err)
	}

	cancel()
	releaseOnce()
	<-done
}

func TestDockerActionImageTag(t *testing.T) {
	assert.Equal(t,
		"act-abc123-dockeraction:latest",
		dockerActionImageTag("owner/repo", "abc123", false),
	)

	assert.Equal(t,
		"act-owner-repo-baca2daaa2fe-dockeraction:latest",
		dockerActionImageTag("owner/repo", "./", true),
	)
	assert.Equal(t,
		"act-owner-repo-sub-e847b61255a8-dockeraction:latest",
		dockerActionImageTag("owner/repo", "./sub", true),
	)

	collisions := [][2]struct {
		repoName   string
		actionName string
	}{
		{{"a/b-c", "./"}, {"a-b/c", "./"}},
		{{"owner/repo-a", "./"}, {"owner/repo", "./a"}},
	}
	for _, c := range collisions {
		assert.NotEqual(t,
			dockerActionImageTag(c[0].repoName, c[0].actionName, true),
			dockerActionImageTag(c[1].repoName, c[1].actionName, true),
			"local docker action tags must differ for %q/%q vs %q/%q",
			c[0].repoName, c[0].actionName, c[1].repoName, c[1].actionName,
		)
	}

	assert.NotEqual(t,
		dockerActionImageTag("owner/repo", "./", true),
		dockerActionImageTag("owner/repo", "./sub", true),
	)

	workflow := &model.Workflow{Jobs: map[string]*model.Job{"job1": {}}}
	job := newTestRC(workflow, nil)
	step := &stepActionRemote{Step: &model.Step{}, RunContext: job}
	image := job.dockerActionImage(t.Context(), step, "abc123", false)
	assert.True(t, strings.HasPrefix(image, "act-abc123-dockeraction:latest-"))
	assert.Equal(t, image, (&RunContext{Parent: job}).dockerActionImage(t.Context(), step, "abc123", false))
	assert.NotEqual(t, image, job.dockerActionImage(t.Context(), &stepActionRemote{Step: &model.Step{}, RunContext: job}, "abc123", false))
	assert.NotEqual(t, image, newTestRC(workflow, nil).dockerActionImage(t.Context(), step, "abc123", false))
	job.dockerActionImage(common.WithDryrun(t.Context(), true), &stepActionRemote{Step: &model.Step{}, RunContext: job}, "abc123", false)
	assert.Len(t, job.dockerActionImages, 2)
}

func TestExecAsDockerStageEntrypoint(t *testing.T) {
	orig := ContainerNewContainer
	defer func() { ContainerNewContainer = orig }()

	for _, tc := range []struct {
		name           string
		stage          stepStage
		with           map[string]string
		inputs         map[string]model.Input
		runs           model.ActionRuns
		env            map[string]string
		wantCmd        []string
		wantEntrypoint []string
		warning        string
	}{
		{
			name:  "main stage prefers manifest values",
			stage: stepStageMain,
			with:  map[string]string{"args": "caller", "entrypoint": "input.sh"},
			runs: model.ActionRuns{
				Entrypoint: "main.sh",
				Args:       []string{"manifest"},
				Env:        map[string]string{"ACTION_ONLY": "manifest"},
			},
			wantCmd:        []string{"manifest"},
			wantEntrypoint: []string{"main.sh"},
		},
		{
			name:           "main stage uses caller fallbacks in any case and warns about undeclared inputs",
			stage:          stepStageMain,
			with:           map[string]string{"ARGS": "caller --flag", "entryPoint": "input.sh --verbose", "unknown": "value"},
			inputs:         map[string]model.Input{"message": {}},
			runs:           model.ActionRuns{Env: map[string]string{"ACTION_ONLY": "manifest"}},
			wantCmd:        []string{"caller", "--flag"},
			wantEntrypoint: []string{"input.sh", "--verbose"},
			warning:        "##[warning]Unexpected input(s) 'unknown', valid inputs are ['entryPoint', 'args', 'message']",
		},
		{
			name:    "manifest args keep caller input quotes literal",
			stage:   stepStageMain,
			with:    map[string]string{"args": "it's literal"},
			runs:    model.ActionRuns{Args: []string{"${{ inputs.args }}"}},
			env:     map[string]string{"INPUT_ARGS": "it's literal"},
			wantCmd: []string{"it's literal"},
		},
		{
			name:    "caller input skips its default expression",
			stage:   stepStageMain,
			with:    map[string]string{"MESSAGE": "provided"},
			inputs:  map[string]model.Input{"message": {Default: "${{ fromJSON('') }}"}},
			runs:    model.ActionRuns{Args: []string{"${{ inputs.message }}"}},
			env:     map[string]string{"INPUT_MESSAGE": "provided"},
			wantCmd: []string{"provided"},
		},
		{
			name:    "explicit empty manifest args suppress caller args",
			stage:   stepStageMain,
			with:    map[string]string{"args": "it's ignored"},
			runs:    model.ActionRuns{Args: []string{}},
			wantCmd: []string{},
		},
		{
			name:  "pre stage keeps step environment",
			stage: stepStagePre,
			with:  map[string]string{"entrypoint": "input.sh"},
			runs: model.ActionRuns{
				PreEntrypoint: "pre.sh --verbose",
				Args:          []string{"hello"},
				Env:           map[string]string{"SHARED": "manifest"},
			},
			env:            map[string]string{"SHARED": "step"},
			wantCmd:        []string{"hello"},
			wantEntrypoint: []string{"pre.sh", "--verbose"},
		},
		{
			name:           "post stage uses manifest entrypoint",
			stage:          stepStagePost,
			runs:           model.ActionRuns{PostEntrypoint: "post.sh", Args: []string{"hello"}},
			wantCmd:        []string{"hello"},
			wantEntrypoint: []string{"post.sh"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, hook := logtest.NewLogger()
			ctx := common.WithLogger(t.Context(), logger)
			tc.runs.Using, tc.runs.Image = "docker", "docker://node:14"
			cm := &containerMock{}
			var input *container.NewContainerInput
			ContainerNewContainer = func(in *container.NewContainerInput) container.ExecutionsEnvironment {
				input = in
				return cm
			}

			step := &stepActionRemote{
				Step: &model.Step{ID: "1", Uses: "org/action@v1", With: tc.with},
				RunContext: &RunContext{
					Config:       &Config{PresetGitHubContext: &model.GithubContext{}},
					Run:          &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
					JobContainer: cm,
				},
				action:       &model.Action{Inputs: tc.inputs, Runs: tc.runs},
				env:          mergeMaps(tc.env),
				remoteAction: mustNewRemoteAction(t, "org/action@v1"),
			}

			cm.On("Pull", false).Return(func(context.Context) error { return nil })
			cm.On("Remove").Return(func(context.Context) error { return nil })
			cm.On("Create", []string(nil), []string(nil)).Return(func(context.Context) error { return nil })
			cm.On("Start", true).Return(func(context.Context) error { return nil })
			cm.On("Close").Return(func(context.Context) error { return nil })

			if tc.stage == stepStageMain {
				require.NoError(t, runActionImpl(step, t.TempDir(), step.remoteAction)(ctx))
			} else {
				require.NoError(t, execAsDocker(ctx, step, "action", t.TempDir(), t.TempDir(), false, tc.stage))
			}
			if tc.warning == "" {
				assert.Empty(t, hook.AllRecords())
			} else {
				require.Len(t, hook.AllRecords(), 1)
				assert.Equal(t, tc.warning, hook.LastRecord().Message)
			}
			require.NotNil(t, input)
			assert.Equal(t, tc.wantCmd, input.Cmd)
			assert.Equal(t, tc.wantEntrypoint, input.Entrypoint)
			for key, value := range mergeMaps(tc.runs.Env, tc.env) {
				assert.Contains(t, input.Env, key+"="+value)
			}
		})
	}
}

func TestDockerActionHasPreAndPostStep(t *testing.T) {
	newStep := func(runs model.ActionRuns) actionStep {
		return &stepActionRemote{action: &model.Action{Runs: runs}}
	}
	ctx := t.Context()

	assert.False(t, hasPreStep(newStep(model.ActionRuns{Using: "docker", Image: "Dockerfile"}))(ctx))
	assert.False(t, hasPostStep(newStep(model.ActionRuns{Using: "docker", Image: "Dockerfile"}))(ctx))

	withStages := model.ActionRuns{Using: "docker", Image: "Dockerfile", PreEntrypoint: "pre.sh", PostEntrypoint: "post.sh"}
	assert.True(t, hasPreStep(newStep(withStages))(ctx))
	assert.True(t, hasPostStep(newStep(withStages))(ctx))
}
