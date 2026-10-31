// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v4"
)

func TestJobExecutor(t *testing.T) {
	t.Parallel()
	// Dryrun only checks syntax/planning; all cases resolve locally, so this runs offline.
	tables := []TestJobFileInfo{
		{workdir, "uses-and-run-in-one-step", "push", "Invalid run/uses syntax for job:test step:Test", platforms, secrets},
		{workdir, "uses-github-empty", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-noref", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-root", "push", "", platforms, secrets},
		{workdir, "uses-docker-url", "push", "", platforms, secrets},
		{workdir, "job-nil-step", "push", "invalid Step 0: missing run or uses key", platforms, secrets},
	}
	// These tests are sufficient to only check syntax.
	ctx := common.WithDryrun(t.Context(), true)
	for _, table := range tables {
		t.Run(table.workflowPath, func(t *testing.T) {
			t.Parallel()
			table.runTest(ctx, t, &Config{})
		})
	}
}

type jobInfoMock struct {
	mock.Mock
}

func (jim *jobInfoMock) matrix() map[string]any {
	args := jim.Called()
	return args.Get(0).(map[string]any)
}

func (jim *jobInfoMock) steps() []*model.Step {
	args := jim.Called()

	return args.Get(0).([]*model.Step)
}

func (jim *jobInfoMock) startContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) stopContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) closeContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) interpolateOutputs() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) result(result string) {
	jim.Called(result)
}

type jobContainerMock struct {
	container.Container
	linuxEnvironment
}

func (jcm *jobContainerMock) ReplaceLogWriter(_, _ io.Writer) (io.Writer, io.Writer) {
	return nil, nil
}

type stepFactoryMock struct {
	mock.Mock
}

func (sfm *stepFactoryMock) newStep(model *model.Step, rc *RunContext) (step, error) {
	args := sfm.Called(model, rc)
	return args.Get(0).(step), args.Error(1)
}

// actionPreparerMock stands in for a step whose action is downloaded before the job's first step.
type actionPreparerMock struct {
	reference string
	sha       string
	prepared  int
}

func (apm *actionPreparerMock) prepareActionExecutor() common.Executor {
	return func(context.Context) error {
		apm.prepared++
		return nil
	}
}

func (apm *actionPreparerMock) actionDownloadInfo() (string, string) {
	return apm.reference, apm.sha
}

func TestPrintPrepareActionsFetchesConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		buf := &bytes.Buffer{}
		ctx := common.WithLogger(t.Context(), logging.New(logging.NewLevelHandler(newJobLogHandler(buf, cyan), logging.LevelInfo)).With("job", "j1"))
		rc := newPrefetchRunContext(t)
		started, release := make(chan string), make(chan struct{})
		var fetched atomic.Int32
		setFetch(t, func(_ctx context.Context, input git.NewGitCloneExecutorInput) (string, string, error) {
			fetched.Add(1)
			started <- input.Ref
			<-release
			return filepath.Join(input.Dir, "commit"), "sha-" + input.Ref, nil
		})
		preparers := append(newRemotePreparers(rc, "org/repo@v1", "org/repo/sub@v1", "org/repo@v2", "org/repo@v3", "org/repo@v4", "org/repo@v5"), &actionPreparerMock{})
		finished := make(chan error)
		go func() { finished <- printPrepareActions(rc, preparers)(ctx) }()
		for range 4 {
			<-started
		}
		synctest.Wait()
		assert.EqualValues(t, 4, fetched.Load())
		close(release)
		<-started
		require.NoError(t, <-finished)
		assert.EqualValues(t, 5, fetched.Load())
		lines := []string{"[j1]   | Prepare all required actions"}
		for _, ref := range []string{"v1", "v2", "v3", "v4", "v5"} {
			lines = append(lines, fmt.Sprintf("[j1]   | Download action repository 'org/repo@%s' (SHA:sha-%s)", ref, ref))
		}
		assert.Equal(t, strings.Join(append(lines, ""), "\n"), buf.String())
	})
}

func TestPrintPrepareActionsFailsJobAndCancelsLaterDownloads(t *testing.T) {
	for name, panicFetch := range map[string]bool{"error": false, "panic": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rc := newPrefetchRunContext(t)
				setFetch(t, func(ctx context.Context, input git.NewGitCloneExecutorInput) (string, string, error) {
					if input.Ref == "v1" {
						if panicFetch {
							panic("broken archive")
						}
						return "", "", assert.AnError
					}
					<-ctx.Done()
					return "", "", ctx.Err()
				})
				ctx := common.WithJobErrorContainer(common.WithLogger(t.Context(), logging.New(logging.DiscardHandler)))
				remaining := &actionPreparerMock{}
				err := printPrepareActions(rc, append(newRemotePreparers(rc, "org/repo@v1", "org/repo@v2"), remaining))(ctx)
				if panicFetch {
					require.ErrorContains(t, err, "panic: broken archive")
				} else {
					require.ErrorIs(t, err, assert.AnError)
				}
				require.ErrorIs(t, err, common.JobError(ctx))
				assert.True(t, rc.jobFailed)
				assert.Zero(t, remaining.prepared)
			})
		})
	}
}

func newPrefetchRunContext(t *testing.T) *RunContext {
	return &RunContext{
		Config: &Config{ActionCacheDir: t.TempDir(), PresetGitHubContext: &model.GithubContext{}},
		Run:    &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
	}
}

func newRemotePreparers(rc *RunContext, uses ...string) []actionPreparer {
	preparers := make([]actionPreparer, 0, len(uses))
	for _, use := range uses {
		preparers = append(preparers, &stepActionRemote{Step: &model.Step{Uses: use}, RunContext: rc, readAction: func(_ctx context.Context, _step *model.Step, _actionDir, _actionPath string, _readFile actionYamlReader, _writeFile fileWriter) (*model.Action, error) {
			return &model.Action{}, nil
		}})
	}
	return preparers
}

func TestPrintPrepareActionsSkipsWithoutActions(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := logging.New(logging.NewLevelHandler(newJobLogHandler(buf, cyan), logging.LevelInfo))
	ctx := common.WithLogger(t.Context(), logger.With("job", "j1"))

	require.NoError(t, printPrepareActions(&RunContext{}, nil)(ctx))

	assert.Empty(t, buf.String())
}

func TestNewStepsExecutorSkipsStepsAfterSetupFailure(t *testing.T) {
	for name, tt := range map[string]struct {
		preErr  error
		mainRan bool
	}{
		"setup failure":    {preErr: setupError{assert.AnError}},
		"pre step failure": {preErr: assert.AnError, mainRan: true}, // always() steps still run, as on GitHub
	} {
		t.Run(name, func(t *testing.T) {
			ctx := common.WithJobErrorContainer(common.WithLogger(context.Background(), logging.New(logtest.New())))
			mainRan := false
			err := newStepsExecutor(&RunContext{},
				[]common.Executor{func(context.Context) error { return tt.preErr }},
				[]common.Executor{func(context.Context) error { mainRan = true; return nil }},
			)(ctx)
			require.ErrorIs(t, err, assert.AnError)
			assert.Equal(t, tt.mainRan, mainRan)
		})
	}
}

func TestPrintCompleteJobName(t *testing.T) {
	for name, tt := range map[string]struct {
		rc   *RunContext
		want string
	}{
		"job name":            {rc: &RunContext{JobName: "lint", Name: "lint-1"}, want: "lint"},
		"falls back to name":  {rc: &RunContext{Name: "lint-1"}, want: "lint-1"},
		"falls back to jobID": {rc: &RunContext{Run: &model.Run{JobID: "lint"}}, want: "lint"},
	} {
		t.Run(name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			logger := logging.New(logging.NewLevelHandler(newJobLogHandler(buf, cyan), logging.LevelInfo))
			ctx := common.WithLogger(t.Context(), logger.With("job", "j1"))

			require.NoError(t, printCompleteJobName(tt.rc)(ctx))

			assert.Equal(t, "[j1]   | Complete job name: "+tt.want+"\n", buf.String())
		})
	}
}

// actionStepMock is a step whose action has to be downloaded before it can run.
type actionStepMock struct {
	*stepMock
	*actionPreparerMock
}

// TestNewJobExecutorDownloadsAllActionsBeforeTheFirstStep pins the shape of the setup section:
// every action is downloaded before any step runs, and the job name closes the section. A pre
// step that downloaded its own action would leave the log interleaved with the downloads.
func TestNewJobExecutorDownloadsAllActionsBeforeTheFirstStep(t *testing.T) {
	ctx := common.WithJobErrorContainer(t.Context())
	jim := &jobInfoMock{}
	sfm := &stepFactoryMock{}
	rc := &RunContext{
		JobContainer: &jobContainerMock{},
		Run: &model.Run{
			JobID: "test",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{"test": {}},
			},
		},
		Config: &Config{PresetGitHubContext: &model.GithubContext{}},
	}
	rc.ExprEval = rc.NewExpressionEvaluator(ctx)

	steps := []*model.Step{{ID: "1"}, {ID: "2"}}
	executorOrder := make([]string, 0)

	jim.On("steps").Return(steps)
	jim.On("matrix").Return(map[string]any{})
	jim.On("startContainer").Return(func(context.Context) error { return nil })
	jim.On("stopContainer").Return(func(context.Context) error { return nil })
	jim.On("closeContainer").Return(func(context.Context) error { return nil })
	jim.On("interpolateOutputs").Return(func(context.Context) error { return nil })
	jim.On("result", "success")

	for _, stepModel := range steps {
		sm := &stepMock{}
		apm := &actionPreparerMock{reference: "actions/checkout@v" + stepModel.ID, sha: "sha" + stepModel.ID}
		sfm.On("newStep", stepModel, rc).Return(&actionStepMock{stepMock: sm, actionPreparerMock: apm}, nil)

		sm.On("pre").Return(func(ctx context.Context) error {
			executorOrder = append(executorOrder, "pre"+stepModel.ID)
			common.Logger(ctx).Infof("Run Pre %s", stepModel.ID)
			return nil
		})
		sm.On("main").Return(func(context.Context) error {
			executorOrder = append(executorOrder, "step"+stepModel.ID)
			return nil
		})
		sm.On("post").Return(func(context.Context) error { return nil })

		defer sm.AssertExpectations(t)
	}

	logger, hook := logtest.NewLogger()
	err := newJobExecutor(jim, sfm, rc)(common.WithLogger(ctx, logger.With("job", "test")))
	require.NoError(t, err)

	assert.Equal(t, []string{"pre1", "pre2", "step1", "step2"}, executorOrder)

	// the setup is logged ahead of the pre steps and the cleanup is tagged, so the reporter can show both as steps of their own
	setup := make([]string, 0)
	for _, entry := range hook.AllRecords() {
		if strings.HasPrefix(entry.Message, "Prepare all required actions") || strings.HasPrefix(entry.Message, "Download action") ||
			strings.HasPrefix(entry.Message, "Complete job name") || strings.HasPrefix(entry.Message, "Run Pre") {
			setup = append(setup, entry.Message)
		}
		if _, ok := entry.Attr("jobResult"); ok || strings.HasPrefix(entry.Message, "Cleaning up container") {
			stage, _ := entry.Attr("stage")
			assert.Equal(t, StageComplete, stage, entry.Message)
		}
	}
	assert.Equal(t, []string{
		"Prepare all required actions",
		"Download action repository 'actions/checkout@v1' (SHA:sha1)",
		"Download action repository 'actions/checkout@v2' (SHA:sha2)",
		"Complete job name: test",
		"Run Pre 1",
		"Run Pre 2",
	}, setup)
}

func TestNewJobExecutor(t *testing.T) {
	table := []struct {
		name          string
		steps         []*model.Step
		preSteps      []bool
		postSteps     []bool
		executedSteps []string
		result        string
		hasError      bool
		output        string
		startError    error
		cancelOnStart bool
		timeout       bool
	}{
		{
			name:          "zeroSteps",
			steps:         []*model.Step{},
			preSteps:      []bool{},
			postSteps:     []bool{},
			executedSteps: []string{},
			result:        "success",
			hasError:      false,
		},
		{
			name: "stepWithoutPrePost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"step1",
				"interpolateOutputs",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithFailure",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"step1",
				"interpolateOutputs",
				"stopContainer",
				"closeContainer",
			},
			result:   "failure",
			hasError: true,
		},
		{
			name: "stepWithPre",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{true},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"step1",
				"interpolateOutputs",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithPost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{true},
			executedSteps: []string{
				"startContainer",
				"step1",
				"post1",
				"interpolateOutputs",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithPreAndPost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{true},
			postSteps: []bool{true},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"step1",
				"post1",
				"interpolateOutputs",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepsWithPreAndPost",
			steps: []*model.Step{{
				ID: "1",
			}, {
				ID: "2",
			}, {
				ID: "3",
			}},
			preSteps:  []bool{true, false, true},
			postSteps: []bool{false, true, true},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"pre3",
				"step1",
				"step2",
				"step3",
				"post3",
				"post2",
				"interpolateOutputs",
				"stopContainer",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name:          "jobOutputExpressionFailure",
			steps:         []*model.Step{{ID: "1"}},
			preSteps:      []bool{false},
			postSteps:     []bool{false},
			executedSteps: []string{"startContainer", "step1", "interpolateOutputs", "stopContainer", "closeContainer"},
			result:        "failure",
			output:        "${{ 'test' != test }}",
		},
		{
			name:          "start failure",
			steps:         []*model.Step{{ID: "1"}},
			executedSteps: []string{"startContainer", "closeContainer"},
			startError:    errors.New("start failed"),
		},
		{
			name:          "cancelled at startup boundary",
			steps:         []*model.Step{{ID: "1"}},
			preSteps:      []bool{false},
			postSteps:     []bool{true},
			executedSteps: []string{"startContainer", "step1", "post1", "interpolateOutputs", "stopContainer", "closeContainer"},
			result:        "cancelled",
			cancelOnStart: true,
		},
		{
			name:          "post steps run after timeout",
			steps:         []*model.Step{{ID: "1"}},
			preSteps:      []bool{false},
			postSteps:     []bool{true},
			executedSteps: []string{"startContainer", "step1", "post1", "interpolateOutputs", "stopContainer", "closeContainer"},
			result:        "failure",
			timeout:       true,
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			ctx := common.WithJobErrorContainer(t.Context())
			cancel := func() {}
			var expire func()
			if tt.timeout {
				deadline := newControllableDeadlineContext(ctx)
				ctx, expire = deadline, deadline.expire
			} else {
				ctx, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			jim := &jobInfoMock{}
			sfm := &stepFactoryMock{}
			rc := &RunContext{
				JobContainer: &jobContainerMock{},
				Run: &model.Run{
					JobID: "test",
					Workflow: &model.Workflow{
						Jobs: map[string]*model.Job{
							"test": {},
						},
					},
				},
				Config: &Config{PresetGitHubContext: &model.GithubContext{}},
			}
			if tt.output != "" {
				require.NoError(t, rc.Run.Job().RawOutputs.Encode(map[string]string{"bad": tt.output}))
			}
			rc.ExprEval = rc.NewExpressionEvaluator(ctx)
			executorOrder := make([]string, 0)

			jim.On("steps").Return(tt.steps)

			if len(tt.steps) > 0 {
				jim.On("startContainer").Return(func(_ context.Context) error {
					executorOrder = append(executorOrder, "startContainer")
					if tt.cancelOnStart {
						cancel()
					}
					return tt.startError
				})
			}

			for i, stepModel := range tt.steps {
				sm := &stepMock{}

				sfm.On("newStep", stepModel, rc).Return(sm, nil)

				sm.On("pre").Return(func(_ctx context.Context) error {
					if tt.preSteps[i] {
						executorOrder = append(executorOrder, "pre"+stepModel.ID)
					}
					return nil
				})

				sm.On("main").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "step"+stepModel.ID)
					if tt.timeout {
						expire()
						return ctx.Err()
					}
					if tt.hasError {
						return errors.New("error")
					}
					return nil
				})

				sm.On("post").Return(func(ctx context.Context) error {
					if tt.postSteps[i] {
						executorOrder = append(executorOrder, "post"+stepModel.ID)
					}
					if tt.timeout {
						require.NoError(t, ctx.Err(), "post step must run against a fresh, non-expired context")
					}
					return nil
				})

				defer sm.AssertExpectations(t)
			}

			if len(tt.steps) > 0 && tt.startError == nil {
				jim.On("matrix").Return(map[string]any{})

				jim.On("interpolateOutputs").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "interpolateOutputs")
					if tt.output != "" {
						return rc.interpolateOutputs()(ctx)
					}
					return nil
				})

				if slices.Contains(tt.executedSteps, "stopContainer") {
					jim.On("stopContainer").Return(func(ctx context.Context) error {
						executorOrder = append(executorOrder, "stopContainer")
						require.NoError(t, ctx.Err())
						_, bounded := ctx.Deadline()
						require.True(t, bounded)
						return nil
					})
				}

				jim.On("result", tt.result)
			}

			if len(tt.steps) > 0 {
				jim.On("closeContainer").Return(func(_ctx context.Context) error {
					executorOrder = append(executorOrder, "closeContainer")
					return nil
				})
			}

			executor := newJobExecutor(jim, sfm, rc)
			err := executor(ctx)
			switch {
			case tt.startError != nil:
				require.ErrorIs(t, err, tt.startError)
			case tt.cancelOnStart:
				require.ErrorIs(t, err, context.Canceled)
			default:
				require.NoError(t, err)
			}
			assert.Empty(t, rc.Run.Job().Outputs["bad"])
			assert.Equal(t, tt.executedSteps, executorOrder)

			jim.AssertExpectations(t)
			sfm.AssertExpectations(t)
		})
	}
}

type controllableDeadlineContext struct {
	context.Context
	done chan struct{}
}

func newControllableDeadlineContext(parent context.Context) *controllableDeadlineContext {
	return &controllableDeadlineContext{Context: parent, done: make(chan struct{})}
}

func (ctx *controllableDeadlineContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *controllableDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *controllableDeadlineContext) expire() {
	close(ctx.done)
}

func TestSetJobResultMatrixContinueOnError(t *testing.T) {
	ctx := t.Context()
	for name, tolerated := range map[string]bool{
		"one tolerated and one firm failure fails the run": false,
		"all tolerated failures do not fail the run":       true,
	} {
		t.Run(name, func(t *testing.T) {
			var job *model.Job
			require.NoError(t, yaml.Unmarshal([]byte("continue-on-error: ${{ matrix.experimental }}\nruns-on: ubuntu-latest"), &job))
			workflow := &model.Workflow{Name: "workflow1", Jobs: map[string]*model.Job{"job1": job}}
			for _, experimental := range []bool{true, tolerated} {
				rc := newTestRC(workflow, map[string]any{"experimental": experimental})
				rc.Config.PresetGitHubContext = &model.GithubContext{}
				setJobResult(ctx, rc, rc, false)
			}

			assert.Equal(t, "failure", job.Result)
			assert.Equal(t, tolerated, job.ContinueOnError)
			err := handleFailure(&model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{{Workflow: workflow, JobID: "job1"}}}}})(ctx)
			if tolerated {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestHasJobSummaryCapability(t *testing.T) {
	assert.True(t, hasJobSummaryCapability("cache,job-summary artifacts"))
	assert.True(t, hasJobSummaryCapability("cache,\njob-summary\tartifacts"))
	assert.False(t, hasJobSummaryCapability("not-job-summary,job-summary-v2"))
}

// fakeRuntimeToken builds a JWT-shaped string whose middle (claims) segment encodes
// the given JobID. The header and signature segments are filler — the runner does not
// verify the signature; the server does.
func fakeRuntimeToken(jobID int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"JobID":%d}`, jobID))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + claims + "." + sig
}

func newJobSummaryRC(env map[string]string, jobContainer container.ExecutionsEnvironment, stepCount int) *RunContext {
	steps := make([]*model.Step, stepCount)
	for i := range steps {
		steps[i] = &model.Step{ID: strconv.Itoa(i)}
	}
	return &RunContext{
		Config:       &Config{},
		JobContainer: jobContainer,
		Env:          env,
		Run: &model.Run{
			JobID: "test",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"test": {Steps: steps},
				},
			},
		},
	}
}

func TestTryUploadJobSummaryRetriesTransientFailure(t *testing.T) {
	oldDelay := jobSummaryUploadRetryDelay
	jobSummaryUploadRetryDelay = 0
	defer func() {
		jobSummaryUploadRetryDelay = oldDelay
	}()

	runtimeToken := fakeRuntimeToken(34)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/_apis/pipelines/workflows/12/jobs/34/steps/0/summary", r.URL.Path)
		assert.Equal(t, "Bearer "+runtimeToken, r.Header.Get("Authorization"))
		assert.Equal(t, "text/markdown; charset=utf-8", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Equal(t, []byte("# summary"), body)
		if requests == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx := t.Context()
	cm := &containerMock{}
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-0.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-0.md", body: "# summary"}))),
		nil,
	).Once()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "cache, job-summary",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      runtimeToken,
		"GITEA_RUN_ID":               "12",
	}, cm, 1)

	tryUploadJobSummary(ctx, rc)

	assert.Equal(t, 2, requests)
	cm.AssertExpectations(t)
}

func TestTryUploadJobSummaryStopsAtPhaseTimeout(t *testing.T) {
	oldPhase := jobSummaryUploadPhaseTimeout
	jobSummaryUploadPhaseTimeout = 100 * time.Millisecond
	defer func() {
		jobSummaryUploadPhaseTimeout = oldPhase
	}()

	runtimeToken := fakeRuntimeToken(34)

	// The server blocks until either the request context is cancelled (the behaviour
	// under test: the phase timeout aborts the in-flight upload) or the test tears it
	// down. Without the phase timeout the upload would hang until the 30s client
	// timeout instead of releasing the cleanup budget. The release channel guarantees
	// the handler always returns so server.Close() cannot itself hang.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	ctx := t.Context()
	cm := &containerMock{}
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-0.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-0.md", body: "# summary"}))),
		nil,
	).Once()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "job-summary",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      runtimeToken,
		"GITEA_RUN_ID":               "12",
	}, cm, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		tryUploadJobSummary(ctx, rc)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tryUploadJobSummary did not honour the phase timeout")
	}
	cm.AssertExpectations(t)
}

func TestTryUploadJobSummaryUploadsEachStepIndependentlyWithInstanceTransport(t *testing.T) {
	runtimeToken := fakeRuntimeToken(34)

	type upload struct {
		path string
		body string
	}
	var got []upload
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		got = append(got, upload{r.URL.Path, string(body)})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx := t.Context()
	cm := &containerMock{}
	// Three steps: 0 has content, 1 has empty content (skipped), 2 has content.
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-0.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-0.md", body: "first"}))),
		nil,
	).Once()
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-1.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-1.md", body: ""}))),
		nil,
	).Once()
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-2.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "step-summary-2.md", body: "third"}))),
		nil,
	).Once()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "job-summary",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      runtimeToken,
		"GITEA_RUN_ID":               "12",
	}, cm, 3)
	rc.Config.GitHubInstance, rc.Config.InstanceTransport = server.URL, server.Client().Transport

	tryUploadJobSummary(ctx, rc)

	assert.Equal(t, []upload{
		{"/_apis/pipelines/workflows/12/jobs/34/steps/0/summary", "first"},
		{"/_apis/pipelines/workflows/12/jobs/34/steps/2/summary", "third"},
	}, got)
	cm.AssertExpectations(t)
}

func TestTryUploadJobSummarySkipsUnsupportedUploads(t *testing.T) {
	for name, tt := range map[string]struct {
		capabilities string
		runtimeToken string
	}{
		"requires exact capability": {"not-job-summary,job-summary-v2", fakeRuntimeToken(34)},
		"requires job ID":           {"job-summary", "not-a-jwt"},
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _r *http.Request) {
				requests++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			rc := newJobSummaryRC(map[string]string{
				"GITEA_ACTIONS_CAPABILITIES": tt.capabilities,
				"ACTIONS_RUNTIME_URL":        server.URL,
				"ACTIONS_RUNTIME_TOKEN":      tt.runtimeToken,
				"GITEA_RUN_ID":               "12",
			}, &containerMock{}, 1)

			tryUploadJobSummary(t.Context(), rc)

			assert.Zero(t, requests)
		})
	}
}

func TestExtractJobIDFromRuntimeToken(t *testing.T) {
	assert.Equal(t, int64(42), extractJobIDFromRuntimeToken(fakeRuntimeToken(42)))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken("not-a-jwt"))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken("a.b.c"))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken(""))
}

func TestReadSingleFileFromContainerArchiveFindsMatchingRegularFile(t *testing.T) {
	ctx := t.Context()
	cm := &containerMock{}
	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/SUMMARY.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t,
			tarEntry{name: "workflow", typeflag: tar.TypeDir},
			tarEntry{name: "other.md", body: "wrong"},
			tarEntry{name: "SUMMARY.md", body: "right"},
		))),
		nil,
	).Once()

	body, ok := readSingleFileFromContainerArchive(ctx, cm, "/var/run/act/workflow/SUMMARY.md", 1024)

	assert.True(t, ok)
	assert.Equal(t, []byte("right"), body)
	cm.AssertExpectations(t)
}

func TestReadSingleFileFromContainerArchiveSizeLimit(t *testing.T) {
	for name, tt := range map[string]struct {
		content  string
		maxBytes int64
		want     string
	}{
		"truncates oversized summary": {strings.Repeat("a", 300), 200, strings.Repeat("a", 200-len(jobSummaryTruncationMarker)) + jobSummaryTruncationMarker},
		"keeps exact limit":           {"abc", 3, "abc"},
	} {
		t.Run(name, func(t *testing.T) {
			logger, hook := logtest.NewLogger()
			ctx := common.WithLogger(t.Context(), logger)
			cm := &containerMock{}
			cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/SUMMARY.md").Return(
				io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "SUMMARY.md", body: tt.content}))),
				nil,
			).Once()

			body, ok := readSingleFileFromContainerArchive(ctx, cm, "/var/run/act/workflow/SUMMARY.md", tt.maxBytes)

			assert.True(t, ok)
			assert.Equal(t, []byte(tt.want), body)
			assert.LessOrEqual(t, int64(len(body)), tt.maxBytes)
			if int64(len(tt.content)) > tt.maxBytes {
				if assert.Len(t, hook.AllRecords(), 1) {
					assert.Contains(t, hook.LastRecord().Message, "job summary truncated")
				}
			} else {
				assert.Empty(t, hook.AllRecords())
			}
			cm.AssertExpectations(t)
		})
	}
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
}

func tarArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()

	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Typeflag: typeflag,
			Mode:     0o644,
			Size:     int64(len(entry.body)),
		}
		if typeflag == tar.TypeDir {
			header.Mode = 0o755
			header.Size = 0
		}
		require.NoError(t, tw.WriteHeader(header))
		if typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(entry.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func newTestRC(wf *model.Workflow, matrix map[string]any) *RunContext {
	return &RunContext{
		Config:      &Config{Workdir: ".", Env: testCheckoutEnv(), PlatformPicker: func([]string) string { return "ubuntu-latest" }},
		StepResults: map[string]*model.StepResult{},
		Env:         map[string]string{},
		Matrix:      matrix,
		Run:         &model.Run{JobID: "job1", Workflow: wf},
	}
}

func makeTestRC(t *testing.T, jobYAML string) *RunContext {
	t.Helper()
	var job *model.Job
	require.NoError(t, yaml.Unmarshal([]byte(jobYAML), &job))
	rc := newTestRC(&model.Workflow{
		Name: "workflow1",
		Jobs: map[string]*model.Job{"job1": job},
	}, nil)
	rc.Config.PresetGitHubContext = &model.GithubContext{}
	rc.ExprEval = rc.NewExpressionEvaluator(t.Context())
	return rc
}

func TestApplyJobTimeout(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"empty", "runs-on: ubuntu-latest", 0},
		{"integer", "timeout-minutes: 5\nruns-on: ubuntu-latest", 5 * time.Minute},
		{"zero ignored", "timeout-minutes: 0\nruns-on: ubuntu-latest", 0},
		{"non-numeric ignored", "timeout-minutes: abc\nruns-on: ubuntu-latest", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := makeTestRC(t, tc.yaml)
			newCtx, cancel := applyJobTimeout(t.Context(), rc, rc.Run.Job())
			defer cancel()
			deadline, hasDeadline := newCtx.Deadline()
			assert.Equal(t, tc.want != 0, hasDeadline)
			if hasDeadline {
				assert.WithinDuration(t, time.Now().Add(tc.want), deadline, time.Minute)
			}
		})
	}
}

func TestEvaluateJobContinueOnError(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"absent", "runs-on: ubuntu-latest", false},
		{"true", "continue-on-error: true\nruns-on: ubuntu-latest", true},
		{"false", "continue-on-error: false\nruns-on: ubuntu-latest", false},
		{"expression true", "continue-on-error: ${{ 'x' == 'x' }}\nruns-on: ubuntu-latest", true},
		{"expression false", "continue-on-error: ${{ 'x' != 'x' }}\nruns-on: ubuntu-latest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := makeTestRC(t, tc.yaml)
			got := evaluateJobContinueOnError(t.Context(), rc, rc.Run.Job())
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestJobSetContinueOnError(t *testing.T) {
	for name, tt := range map[string]struct {
		values []bool
		want   bool
	}{
		"first call true":                {[]bool{true}, true},
		"first call false":               {[]bool{false}, false},
		"true then false locks to false": {[]bool{true, false}, false},
		"false then true stays false":    {[]bool{false, true}, false},
		"true then true stays true":      {[]bool{true, true}, true},
	} {
		t.Run(name, func(t *testing.T) {
			job := &model.Job{}
			for _, value := range tt.values {
				job.SetContinueOnError(value)
			}
			assert.Equal(t, tt.want, job.ContinueOnError)
		})
	}
}

func TestTryUploadJobSummaryMasksSecrets(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		got = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cm := &containerMock{}
	cm.On("GetContainerArchive", mock.Anything, "/var/run/act/workflow/step-summary-0.md").Return(
		io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{
			name: "step-summary-0.md", body: "deployed true with s3cr3t and runtime-added via pr0xypw",
		}))),
		nil,
	).Once()

	rc := newJobSummaryRC(map[string]string{
		"GITEA_ACTIONS_CAPABILITIES": "job-summary",
		"ACTIONS_RUNTIME_URL":        server.URL,
		"ACTIONS_RUNTIME_TOKEN":      fakeRuntimeToken(34),
		"GITEA_RUN_ID":               "12",
	}, cm, 1)
	rc.Config.Secrets = map[string]string{"TOK": "s3cr3t", "ACTIONS_STEP_DEBUG": "true"}
	rc.Config.ExtraMasks = []string{"pr0xypw"}
	rc.Masks = []string{"runtime-added"}

	tryUploadJobSummary(t.Context(), rc)

	assert.Equal(t, "deployed true with *** and *** via ***", got)
	cm.AssertExpectations(t)
}
