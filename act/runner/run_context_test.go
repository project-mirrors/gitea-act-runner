// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"github.com/moby/moby/api/types/volume"
	assert "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	require "github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v4"
)

func TestRunContext_EvalBool(t *testing.T) {
	rc := &RunContext{
		Config: &Config{
			Workdir: ".",
			Env:     testCheckoutEnv(),
		},
		Env: map[string]string{
			"SOMETHING_TRUE":  "true",
			"SOMETHING_FALSE": "false",
			"SOME_TEXT":       "text",
		},
		Run: &model.Run{
			JobID: "job1",
			Workflow: &model.Workflow{
				Name: "test-workflow",
				Jobs: map[string]*model.Job{
					"job1": {},
				},
			},
		},
		StepResults: map[string]*model.StepResult{
			"id1": {Conclusion: model.StepStatusSuccess, Outcome: model.StepStatusFailure},
		},
	}
	rc.ExprEval = rc.NewExpressionEvaluator(t.Context())

	tables := []struct {
		in      string
		out     bool
		wantErr bool
	}{
		{in: "failure()", out: false},
		{in: "success()", out: true},
		{in: "cancelled()", out: false},
		{in: "always()", out: true},
		{in: "true", out: true},
		{in: "false", out: false},
		{in: "1 != 0", out: true},
		{in: "1 != 1", out: false},
		{in: "${{ 1 != 0 }}", out: true},
		{in: "${{ 1 != 1 }}", out: false},
		{in: "1 == 0", out: false},
		{in: "1 == 1", out: true},
		{in: "1 > 2", out: false},
		{in: "1 < 2", out: true},
		{in: "true && false", out: false},
		{in: "true && 1 < 2", out: true},
		{in: "false || 1 < 2", out: true},
		{in: "false || false", out: false},
		{in: "env.UNKNOWN == 'true'", out: false},
		{in: "env.UNKNOWN", out: false},
		{in: "env.SOME_TEXT", out: true},
		{in: "env.SOME_TEXT == 'text'", out: true},
		{in: "env.SOMETHING_TRUE == 'true'", out: true},
		{in: "env.SOMETHING_FALSE == 'true'", out: false},
		{in: "env.SOMETHING_TRUE", out: true},
		{in: "env.SOMETHING_FALSE", out: true},
		{in: "${{ !env.SOMETHING_TRUE }}", out: false},
		{in: "${{ !env.SOMETHING_FALSE }}", out: false},
		{in: "${{ ! env.SOMETHING_TRUE }}", out: false},
		{in: "${{ ! env.SOMETHING_FALSE }}", out: false},
		{in: "${{ env.SOMETHING_TRUE }}", out: true},
		{in: "${{ env.SOMETHING_FALSE }}", out: true},
		{in: "${{ !env.SOMETHING_TRUE && true }}", out: false},
		{in: "${{ !env.SOMETHING_FALSE && true }}", out: false},
		{in: "${{ !env.SOMETHING_TRUE || true }}", out: true},
		{in: "${{ !env.SOMETHING_FALSE || false }}", out: false},
		{in: "${{ env.SOMETHING_TRUE && true }}", out: true},
		{in: "${{ env.SOMETHING_FALSE || true }}", out: true},
		{in: "${{ env.SOMETHING_FALSE || false }}", out: true},
		{in: "${{ env.SOMETHING_TRUE == 'true'}}", out: true},
		{in: "${{ env.SOMETHING_FALSE == 'true'}}", out: false},
		{in: "${{ env.SOMETHING_FALSE == 'false'}}", out: true},
		{in: "${{ env.SOMETHING_FALSE }} && ${{ env.SOMETHING_TRUE }}", out: true},

		{in: "false || env.SOMETHING_TRUE == 'true'", out: true},
		{in: "true || env.SOMETHING_FALSE == 'true'", out: true},
		{in: "true && env.SOMETHING_TRUE == 'true'", out: true},
		{in: "false && env.SOMETHING_TRUE == 'true'", out: false},
		{in: "env.SOMETHING_FALSE == 'true' && env.SOMETHING_TRUE == 'true'", out: false},
		{in: "env.SOMETHING_FALSE == 'true' && true", out: false},
		{in: "${{ env.SOMETHING_FALSE == 'true' }} && true", out: true},
		{in: "true && ${{ env.SOMETHING_FALSE == 'true' }}", out: true},
		{in: "github.actor == 'nektos/act'", out: true},
		{in: "github.actor == 'unknown'", out: false},
		{in: "github.job == 'job1'", out: true},
		{in: "${{ env.ACT }}", out: true},
		{in: "${{ !env.ACT }}", out: false},
		{in: "INVALID_EXPRESSION", wantErr: true},
	}

	for _, table := range tables {
		t.Run(table.in, func(t *testing.T) {
			b, err := EvalBool(t.Context(), rc.ExprEval, table.in, exprparser.DefaultStatusCheckSuccess)
			if table.wantErr {
				require.Error(t, err)
			}

			assert.Equal(t, table.out, b)
		})
	}
}

type fakeContainer struct {
	container.ExecutionsEnvironment
	caseInsensitive bool
}

func (c fakeContainer) IsEnvironmentCaseInsensitive() bool { return c.caseInsensitive }

func (fakeContainer) Pull(bool) common.Executor { return func(context.Context) error { return nil } }

func (fakeContainer) Start(bool) common.Executor { return func(context.Context) error { return nil } }

func (fakeContainer) Remove() common.Executor { return func(context.Context) error { return nil } }

func (fakeContainer) Close() common.Executor             { return func(context.Context) error { return nil } }
func (fakeContainer) GetActPath() string                 { return "/var/run/act" }
func (fakeContainer) ToContainerPath(path string) string { return path }
func (fakeContainer) Create([]string, []string) common.Executor {
	return func(context.Context) error { return nil }
}

func (fakeContainer) Copy(string, ...*container.FileEntry) common.Executor {
	return func(context.Context) error { return nil }
}

func (fakeContainer) Inspect(context.Context) (*container.Info, error) {
	return &container.Info{ID: "fake", State: "running", Health: container.HealthNone}, nil
}

func (fakeContainer) DumpLogs(context.Context) error { return nil }

func fakeDockerDaemon(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/_ping") {
			writer.Header().Set("API-Version", "1.47")
		} else {
			handler(writer, request)
		}
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("DOCKER_HOST", daemon.URL)
}

func startJobContainerInputs(t *testing.T, workflowYAML string, cfg *Config) []*container.NewContainerInput {
	t.Helper()
	workflow, err := model.ReadWorkflow(strings.NewReader(workflowYAML))
	require.NoError(t, err)

	var inputs []*container.NewContainerInput
	origNewContainer := newContainer
	newContainer = func(input *container.NewContainerInput) container.ExecutionsEnvironment {
		inputs = append(inputs, input)
		return fakeContainer{}
	}
	t.Cleanup(func() { newContainer = origNewContainer })

	cfg.Workdir = "/tmp"
	cfg.ContainerNetworkMode = "host"
	cfg.Env = testCheckoutEnv()
	cfg.Secrets = map[string]string{}

	rc := &RunContext{
		Name:   "test",
		Config: cfg,
		Run: &model.Run{
			JobID:    "job",
			Workflow: workflow,
		},
	}
	rc.ExprEval = rc.NewExpressionEvaluator(t.Context())
	require.NoError(t, rc.resolvePlatformImage(t.Context()))

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.47")
			_, _ = io.WriteString(w, "OK")
		case strings.HasSuffix(r.URL.Path, "/containers/json"), strings.HasSuffix(r.URL.Path, "/networks"):
			_, _ = io.WriteString(w, "[]")
		case strings.HasSuffix(r.URL.Path, "/volumes"):
			_, _ = io.WriteString(w, `{"Volumes":[]}`)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/volumes/"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = io.WriteString(w, `{"Architecture":"amd64","OSType":"linux"}`)
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("DOCKER_HOST", daemon.URL)
	require.NoError(t, rc.startJobContainer()(t.Context()))

	return inputs
}

func TestStartJobContainerKeepsJobCredentialsWithServices(t *testing.T) {
	inputs := startJobContainerInputs(t, `
name: test
on: push
jobs:
  job:
    runs-on: ubuntu-latest
    container:
      image: registry.example/private:latest
      credentials:
        username: job-user
        password: job-password
    services:
      redis:
        image: redis:latest
      db:
        image: postgres:latest
        credentials:
          username: db-user
          password: db-password
    steps: []
`, &Config{})

	credentials := map[string][2]string{}
	for _, in := range inputs {
		credentials[in.Image] = [2]string{in.Username, in.Password}
	}

	require.Equal(t, [2]string{"job-user", "job-password"}, credentials["registry.example/private:latest"])
	require.Equal(t, [2]string{"db-user", "db-password"}, credentials["postgres:latest"])
	require.Equal(t, [2]string{"", ""}, credentials["redis:latest"])
}

func TestStartJobContainerGivesServicesTheirVolumes(t *testing.T) {
	redis := startJobContainerInputs(t, `
jobs:
  job:
    services:
      redis:
        image: redis:latest
        volumes:
          - data:/data
          - data:/backup
`, &Config{ValidVolumes: []string{"data"}})[0]

	require.Equal(t, "redis:latest", redis.Image)
	require.Equal(t, []string{"data"}, redis.ValidVolumes)
	require.Equal(t, map[string]string{"data": "/data"}, redis.Mounts)
	require.Equal(t, []string{"data:/backup"}, redis.Binds)
	require.Empty(t, redis.WorkingDir)
	require.Nil(t, redis.Entrypoint)
	require.Nil(t, redis.Cmd)
}

func TestStartJobContainerBuildsEvaluatedJobAndServiceInputs(t *testing.T) {
	inputs := startJobContainerInputs(t, `
jobs:
  job:
    container: ${{ fromJSON('{"image":"docker://node:20","options":"--label ${{ github.job }}","credentials":{"username":"user","password":"${{ vars.TITLE }}"}}') }}
    services: ${{ fromJSON('{"redis":{"image":"docker://redis:latest","volumes":["data:/data"],"env":{"TITLE":"${{ github.job }}"},"credentials":{"username":"user","password":"${{ github.job }}"},"entrypoint":"/entry.sh","command":"redis-server --port 6380"}}') }}
`, &Config{})
	redis, job := inputs[0], inputs[1]

	require.Equal(t, "redis:latest", redis.Image)
	require.Equal(t, map[string]string{"data": "/data"}, redis.Mounts)
	require.Equal(t, []string{"TITLE=${{ github.job }}"}, redis.Env)
	require.Equal(t, "${{ github.job }}", redis.Password)
	require.Equal(t, []string{"/entry.sh"}, redis.Entrypoint)
	require.Equal(t, []string{"redis-server", "--port", "6380"}, redis.Cmd)
	require.Equal(t, "node:20", job.Image)
	require.Equal(t, "sleep", job.Entrypoint[0])
	require.Equal(t, "--label ${{ github.job }}", job.WorkflowOptions)
	require.Equal(t, "${{ vars.TITLE }}", job.Password)

	inputs = startJobContainerInputs(t, `
env:
  REG_USER: ${{ vars.REG_USER }}
  TEXT: ${{ vars.TEXT }}
  JOB_CONTAINER_NAME: from-workflow
jobs:
  job:
    container:
      image: node:20
      credentials: {username: "${{ env.REG_USER }}", password: pass}
    services:
      redis:
        image: redis:latest
        credentials: {username: "${{ env.REG_USER }}", password: pass}
        env: {TEXT: "${{ env.TEXT }}"}
        options: --volumes-from ${{ env.JOB_CONTAINER_NAME }}
`, &Config{Vars: map[string]string{"REG_USER": "user", "TEXT": "${{ 'literal' }}"}})
	redis, job = inputs[0], inputs[1]
	require.Equal(t, "user", job.Username)
	require.Equal(t, "user", redis.Username)
	require.Equal(t, []string{"TEXT=${{ 'literal' }}"}, redis.Env)
	require.Equal(t, "--volumes-from "+job.Name, redis.WorkflowOptions)
}

func TestStartJobContainerKeepsRunnerOptionsApartFromWorkflowOptions(t *testing.T) {
	inputs := startJobContainerInputs(t, `
name: test
on: push
jobs:
  job:
    runs-on: ubuntu-latest
    container:
      image: registry.example/job:latest
      options: --cap-add SYS_PTRACE
    services:
      redis:
        image: redis:latest
        options: --shm-size 1g
    steps: []
`, &Config{ContainerOptions: "--device /dev/fuse"})

	options := map[string][2]string{}
	for _, in := range inputs {
		options[in.Image] = [2]string{in.RunnerOptions, in.WorkflowOptions}
	}

	require.Equal(t, [2]string{"--device /dev/fuse", "--cap-add SYS_PTRACE"}, options["registry.example/job:latest"])
	require.Equal(t, [2]string{"", "--shm-size 1g"}, options["redis:latest"])
}

func TestStartJobContainerGivesServicesTheJobProxy(t *testing.T) {
	inputs := startJobContainerInputs(t, `
name: test
on: push
jobs:
  job:
    runs-on: ubuntu-latest
    container:
      image: registry.example/job:latest
    services:
      redis:
        image: redis:latest
      db:
        image: postgres:latest
        env:
          no_proxy: db-only.example
    steps: []
`, &Config{ProxyEnv: map[string]string{"http_proxy": "http://proxy:3128", "no_proxy": "internal.example"}})

	db, redis := inputs[0], inputs[1]

	require.Contains(t, redis.Env, "http_proxy=http://proxy:3128")
	require.Contains(t, redis.Env, "no_proxy=internal.example")
	require.Contains(t, db.Env, "no_proxy=db-only.example")
	require.NotContains(t, db.Env, "no_proxy=internal.example")
	require.Contains(t, db.Env, "http_proxy=http://proxy:3128")
}

func TestProxyBuildArgs(t *testing.T) {
	rc := &RunContext{Config: &Config{ProxyEnv: map[string]string{"http_proxy": "http://proxy:3128"}}}

	args := rc.proxyBuildArgs()

	require.Len(t, args, 1)
	require.Equal(t, "http://proxy:3128", *args["http_proxy"])

	require.Nil(t, (&RunContext{Config: &Config{}}).proxyBuildArgs())
}

func TestRunContext_GetBindsAndMounts(t *testing.T) {
	newRunContext := func(config *Config) *RunContext {
		return &RunContext{
			Name: "TestRCName",
			Run: &model.Run{
				Workflow: &model.Workflow{Name: "TestWorkflowName"},
			},
			Config: config,
		}
	}
	rctemplate := newRunContext(&Config{})

	tests := []struct {
		windowsPath bool
		name, want  string
	}{
		{false, "/mnt/linux", "/mnt/linux"},
		{false, "/mnt/path with spaces/linux", "/mnt/path with spaces/linux"},
		{true, `C:\Users\TestPath\MyTestPath`, "/mnt/c/Users/TestPath/MyTestPath"},
		{true, `C:\Users\Test Path with Spaces\MyTestPath`, "/mnt/c/Users/Test Path with Spaces/MyTestPath"},
		{true, "/LinuxPathOnWindowsShouldFail", ""},
	}

	for _, testcase := range tests {
		if (runtime.GOOS == "windows") != testcase.windowsPath {
			continue
		}
		for _, bindWorkDir := range []bool{true, false} {
			testBindSuffix := ""
			if bindWorkDir {
				testBindSuffix = "Bind"
			}
			t.Run(testcase.name+testBindSuffix, func(t *testing.T) {
				rctemplate.Config.Workdir = testcase.name
				rctemplate.Config.BindWorkdir = bindWorkDir
				gotbind, gotmount := rctemplate.GetBindsAndMounts(t.Context())

				if bindWorkDir {
					fullBind := testcase.name + ":" + testcase.want
					if runtime.GOOS == "darwin" {
						fullBind += ":delegated"
					}
					assert.Contains(t, gotbind, fullBind)
				} else {
					assert.Equal(t, testcase.want, gotmount[rctemplate.jobContainerName()])
				}
			})
		}
	}

	t.Run("ContainerVolumeMountTest", func(t *testing.T) {
		tests := []struct {
			name      string
			volumes   []string
			wantbind  string
			wantmount map[string]string
		}{
			{"BindAnonymousVolume", []string{"/volume"}, "/volume", map[string]string{}},
			{"BindHostFile", []string{"/path/to/file/on/host:/volume"}, "/path/to/file/on/host:/volume", map[string]string{}},
			{"MountExistingVolume", []string{"volume-id:/volume"}, "", map[string]string{"volume-id": "/volume"}},
			{"MountExistingVolumeTwice", []string{"volume-id:/volume", "volume-id:/other"}, "volume-id:/other", map[string]string{"volume-id": "/volume"}},
			{"MountExistingVolumeReadOnly", []string{"volume-id:/volume:ro"}, "volume-id:/volume:ro", map[string]string{}},
			{"BindRelativeHostPath", []string{"./relative:/volume"}, "./relative:/volume", map[string]string{}},
			{"OverridesToolCache", []string{"/host/tools:/opt/hostedtoolcache"}, "/host/tools:/opt/hostedtoolcache", map[string]string{}},
			{"OverridesDockerSocket", []string{"/host/docker.sock:/var/run/docker.sock"}, "/host/docker.sock:/var/run/docker.sock", map[string]string{}},
		}

		t.Run("InterpolatedContainerVolumes", func(t *testing.T) {
			job := &model.Job{}
			err := job.RawContainer.Encode(map[string]any{
				"image":   "node:20",
				"volumes": []string{"${{ secrets.MAME }}:/root/.mame/roms:ro"},
			})
			require.NoError(t, err)

			rc := newRunContext(&Config{
				Env:          testCheckoutEnv(),
				ValidVolumes: []string{"**"},
				Secrets: map[string]string{
					"MAME": "/host/mame/roms",
				},
			})
			rc.Run.JobID = "job1"
			rc.Run.Workflow.Jobs = map[string]*model.Job{"job1": job}
			rc.ExprEval = rc.NewExpressionEvaluator(t.Context())
			require.NoError(t, rc.resolvePlatformImage(t.Context()))

			gotbind, gotmount := rc.GetBindsAndMounts(t.Context())
			assert.Contains(t, gotbind, "/host/mame/roms:/root/.mame/roms:ro")
			assert.NotContains(t, gotbind, "${{ secrets.MAME }}")
			assert.NotContains(t, gotmount, "${{ secrets.MAME }}")
		})

		for _, testcase := range tests {
			t.Run(testcase.name, func(t *testing.T) {
				rc := newRunContext(&Config{
					SharedToolCache: true,
					ValidVolumes:    []string{"**"},
				})
				rc.containerSpec.Volumes = testcase.volumes
				rc.Run.JobID = "job1"

				gotbind, gotmount := rc.GetBindsAndMounts(t.Context())

				if len(testcase.wantbind) > 0 {
					assert.Contains(t, gotbind, testcase.wantbind)
				}

				for k, v := range testcase.wantmount {
					assert.Contains(t, gotmount, k)
					assert.Equal(t, gotmount[k], v)
				}

				targets := map[string]bool{}
				for _, bind := range gotbind {
					parsed, err := container.ParseVolume(bind)
					require.NoError(t, err)
					assert.NotContains(t, targets, parsed.Target, "%s mounts an already mounted target", bind)
					targets[parsed.Target] = true
				}
				for source, target := range gotmount {
					assert.NotContains(t, targets, target, "%s mounts an already mounted target", source)
					targets[target] = true
				}
			})
		}
	})

	t.Run("ToolCacheMount", func(t *testing.T) {
		rc := newRunContext(&Config{})

		_, gotmount := rc.GetBindsAndMounts(t.Context())
		assert.NotContains(t, gotmount, sharedToolCacheVolume)

		rc.Config.SharedToolCache = true
		rc.containerSpec.Volumes = []string{"/unapproved:" + container.DefaultToolCache}
		_, gotmount = rc.GetBindsAndMounts(t.Context())
		assert.Equal(t, container.DefaultToolCache, gotmount[sharedToolCacheVolume])
	})

	t.Run("DaemonMountsAboveOwnerRepo", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("container paths are linux paths")
		}
		rc := &RunContext{
			Run:    &model.Run{JobID: "job1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job1": {}}}},
			Config: &Config{BindWorkdir: true, Workdir: "/workspace/1/owner/repo", PresetGitHubContext: &model.GithubContext{}, ValidVolumes: []string{"claimed"}},
		}

		gotbind, _ := rc.GetBindsAndMounts(t.Context())
		assert.True(t, slices.ContainsFunc(gotbind, func(bind string) bool { return strings.HasPrefix(bind, "/workspace/1:/workspace/1") }), gotbind)

		rc.Config.BindWorkdir = false
		_, gotmount := rc.GetBindsAndMounts(t.Context())
		assert.Equal(t, "/workspace/1", gotmount[rc.jobContainerName()])

		rc.containerSpec.Volumes = []string{"claimed:/workspace/1"}
		_, gotmount = rc.GetBindsAndMounts(t.Context())
		assert.Equal(t, "/workspace/1/owner/repo", gotmount[rc.jobContainerName()])
	})
}

func TestRunContextValidVolumes(t *testing.T) {
	rc := &RunContext{
		Name:   "job",
		Run:    &model.Run{Workflow: &model.Workflow{Name: "wf"}},
		Config: &Config{ValidVolumes: []string{"my-vol", "/host/path"}, SharedToolCache: true},
	}
	name := rc.jobContainerName()

	got := rc.validVolumes()

	assert.Subset(t, got, []string{"my-vol", "/host/path", sharedToolCacheVolume, name, name + "-env", "/var/run/docker.sock"})

	assert.Equal(t, []string{"my-vol", "/host/path"}, rc.Config.ValidVolumes)
	assert.Len(t, rc.validVolumes(), len(got), "repeated calls must be stable, not accumulate")

	rc.Config.SharedToolCache = false
	assert.NotContains(t, rc.validVolumes(), sharedToolCacheVolume)

	rc.Config.Workdir = "/workspace/1/owner/repo"
	assert.NotContains(t, rc.validVolumes(), rc.Config.Workdir)
	rc.Config.BindWorkdir = true
	assert.Contains(t, rc.validVolumes(), rc.Config.Workdir)
	rc.Config.PresetGitHubContext = &model.GithubContext{}
	assert.Contains(t, rc.validVolumes(), filepath.FromSlash("/workspace/1"))

	hostEnv := &container.HostEnvironment{Path: t.TempDir(), ActPath: t.TempDir()}
	rc.JobContainer = hostEnv
	rc.Config.BindWorkdir = false
	rc.Config.SharedToolCache = true
	binds, mounts := rc.GetBindsAndMounts(t.Context())
	assert.Subset(t, binds, []string{hostEnv.Path + ":" + hostEnv.Path, hostEnv.GetActPath() + ":" + hostEnv.GetActPath()})
	assert.Contains(t, mounts, sharedToolCacheVolume)
	assert.Equal(t, binds, container.SanitizeBinds(t.Context(), rc.validVolumes(), binds))
}

func TestCleanupJobResourcesCleansServicesWithoutJobContainer(t *testing.T) {
	service := &containerMock{}
	service.On("Remove").Return(func(context.Context) error { return nil }).Once()
	service.On("Close").Return(func(context.Context) error { return nil }).Once()

	rc := &RunContext{
		Config:            &Config{},
		serviceContainers: []*serviceContainer{{name: "svc", container: service}},
	}

	err := rc.cleanupJobResources("external-network", false, true)(common.WithDryrun(t.Context(), true))
	require.NoError(t, err)
	service.AssertExpectations(t)
}

func TestCleanupJobVolumesReapsAbandonedDeferredCleanup(t *testing.T) {
	now := time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)
	rc := &RunContext{
		Config:       &Config{ContainerNetworkCreateOptions: container.NewDockerNetworkCreateExecutorInput{RunnerUUID: "runner-1"}},
		Run:          &model.Run{Workflow: &model.Workflow{Name: "workflow"}, JobID: "job"},
		JobContainer: fakeContainer{},
	}
	volumes := map[string]volume.Volume{}
	fakeDockerDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
		path := strings.TrimPrefix(request.URL.Path, "/v1.47")
		switch {
		case path == "/volumes/create":
			options := volume.Volume{CreatedAt: now.Format(time.RFC3339)}
			assert.NoError(t, json.UnmarshalRead(request.Body, &options))
			volumes[options.Name] = options
			assert.NoError(t, json.MarshalWrite(writer, volumes[options.Name]))
		case path == "/volumes":
			assert.JSONEq(t, `{"label":{"com.gitea.runner.uuid=runner-1":true},"dangling":{"true":true}}`, request.URL.Query().Get("filters"))
			assert.NoError(t, json.MarshalWrite(writer, map[string]any{"Volumes": slices.Collect(maps.Values(volumes))}))
		case request.Method == http.MethodDelete:
			name := strings.TrimPrefix(path, "/volumes/")
			assert.NotContains(t, []string{"1", "true"}, request.URL.Query().Get("force"))
			if name == "became-active" || name == "remove-failed" {
				writer.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(writer, `{"message":%q}`, name)
				return
			}
			delete(volumes, name)
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.URL)
		}
	})
	name := rc.jobContainerName()
	require.NoError(t, rc.createJobVolumes(t.Context(), map[string]string{name: "/workspace", name + "-env": "/var/run/act", "shared-cache": "/cache"}))
	assert.ElementsMatch(t, []string{name, name + "-env"}, slices.Collect(maps.Keys(volumes)))
	rc.deferVolumeCleanup = func(common.Executor) {}
	require.NoError(t, rc.cleanupJobResources("", false, false)(t.Context()))
	require.Len(t, volumes, 2)
	for _, name := range []string{"became-active", "remove-failed"} {
		volumes[name] = volume.Volume{Name: name, Labels: volumes[rc.jobContainerName()].Labels, CreatedAt: now.Format(time.RFC3339)}
	}
	volumes["foreign"] = volume.Volume{Name: "foreign", Labels: map[string]string{"com.gitea.runner.uuid": "runner-2"}, CreatedAt: now.Format(time.RFC3339)}
	volumes["fresh"] = volume.Volume{Name: "fresh", Labels: volumes[name].Labels, CreatedAt: now.Add(48 * time.Hour).Format(time.RFC3339)}
	volumes["unknown-age"] = volume.Volume{Name: "unknown-age", Labels: volumes[name].Labels}
	err := container.RemoveOrphanJobVolumes(t.Context(), "runner-1", now.Add(24*time.Hour))
	require.ErrorContains(t, err, "became-active")
	require.ErrorContains(t, err, "remove-failed")
	assert.ElementsMatch(t, []string{"became-active", "remove-failed", "foreign", "fresh", "unknown-age"}, slices.Collect(maps.Keys(volumes)))
}

func TestRemoveDockerActionImagesByTag(t *testing.T) {
	var removed []string
	fakeDockerDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			removed = append(removed, strings.TrimPrefix(request.URL.Path, "/v1.47/images/"))
			_, _ = io.WriteString(writer, "[]")
			return
		}
		_, _ = io.WriteString(writer, `{"Id":"sha256:shared"}`)
	})
	(&RunContext{dockerActionImages: map[*model.Step]string{{}: "act-action-dockeraction:latest-job"}}).removeDockerActionImages(t.Context())
	assert.Equal(t, []string{"act-action-dockeraction:latest-job"}, removed)
}

func TestCleanupJobResources(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	proxyDir := t.TempDir()
	for _, name := range []string{"synchronous", "proxy", "closed proxy", "kept proxy", "deferred", "preclean deferred", "canceled proxy"} {
		t.Run(name, func(t *testing.T) {
			proxy, preclean, deferred := strings.HasSuffix(name, "proxy"), strings.HasPrefix(name, "preclean"), strings.HasSuffix(name, "deferred")
			removesLabelled := (proxy || preclean) && name != "kept proxy"
			if proxy && runtime.GOOS == "windows" {
				t.Skip("Unix socket ownership is unavailable on Windows")
			}
			jobError, removeError, closeError := errors.New("remove job"), errors.New("remove service"), errors.New("close service")
			job, service := &containerMock{}, &containerMock{}
			removeAlongside := func(started, other chan struct{}, failure error) func(context.Context) error {
				return func(context.Context) error {
					close(started)
					select {
					case <-other:
					case <-time.After(10 * time.Second):
						t.Error("the containers are removed one after the other instead of alongside each other")
					}
					return failure
				}
			}
			jobRemoving, serviceRemoving := make(chan struct{}), make(chan struct{})
			job.On("Remove").Return(removeAlongside(jobRemoving, serviceRemoving, jobError)).Once()
			service.On("Remove").Return(removeAlongside(serviceRemoving, jobRemoving, removeError)).Once()
			service.On("Close").Return(func(context.Context) error { return closeError }).Once()
			rc := &RunContext{
				Config:            &Config{CacheContainer: "cache-container", NoSweep: name == "kept proxy"},
				Run:               &model.Run{Workflow: &model.Workflow{Name: "wf"}, JobID: "job"},
				JobContainer:      job,
				serviceContainers: []*serviceContainer{{name: "svc", container: service}},
			}
			var volumeCleanup []common.Executor
			if deferred {
				rc.deferVolumeCleanup = func(cleanup common.Executor) { volumeCleanup = append(volumeCleanup, cleanup) }
			}
			volumeRemovals := 0
			fakeDockerDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodDelete {
					volumeRemovals++
				}
				operation := request.Method + " " + strings.TrimPrefix(request.URL.Path, "/v1.47")
				if request.URL.Query().Has("filters") {
					operation = "labelled " + operation
				}
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprintf(writer, `{"message":%q}`, operation)
			})
			if proxy {
				listener, err := net.Listen("unix", filepath.Join(proxyDir, "d.sock"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, listener.Close()) })
				rc.dockerProxy, err = container.StartDockerProxy(listener.Addr().String(), proxyDir, rc.jobContainerName())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, rc.closeDockerProxy(context.Background())) })
				if name == "closed proxy" {
					require.NoError(t, rc.closeDockerProxy(t.Context()))
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "canceled proxy" {
				cancel()
			}
			err := rc.cleanupJobResources("job-network", true, preclean)(ctx)
			if deferred && !preclean {
				require.Len(t, volumeCleanup, 1)
				assert.Zero(t, volumeRemovals)
				err = errors.Join(err, volumeCleanup[0](ctx))
			} else {
				assert.Empty(t, volumeCleanup)
			}
			for _, failure := range []error{jobError, removeError, closeError} {
				require.ErrorIs(t, err, failure)
			}
			for _, operation := range []string{"DELETE /volumes/" + rc.jobContainerName(), "DELETE /volumes/" + rc.jobContainerName() + "-env", "POST /networks/job-network/disconnect", "GET /networks"} {
				require.ErrorContains(t, err, operation)
			}
			assert.Equal(t, 2, volumeRemovals)
			assert.Equal(t, removesLabelled, strings.Contains(err.Error(), "labelled GET /containers/json"))
			if removesLabelled {
				require.ErrorContains(t, err, "labelled GET /networks")
				require.ErrorContains(t, err, "labelled GET /volumes")
			}
			assert.Nil(t, rc.dockerProxy)
			assert.Equal(t, proxy, rc.hadDockerProxy)
		})
	}
}

func TestInterpolateOutputsIsPerMatrixComboKeepingNeedsOutputs(t *testing.T) {
	job := &model.Job{Outputs: map[string]string{"o": "${{ matrix.v }}"}}
	require.NoError(t, job.RawOutputs.Encode(job.Outputs))
	needed := &model.Job{Outputs: map[string]string{"o": "from-gitea"}}
	run := &model.Run{JobID: "j", Workflow: &model.Workflow{Name: "w", Jobs: map[string]*model.Job{"j": job, "needed": needed}}}
	r := &runnerImpl{config: &Config{PresetGitHubContext: &model.GithubContext{}}}
	ctx := t.Context()

	_, err := r.newRunContext(ctx, &model.Run{JobID: "needed", Workflow: run.Workflow}, nil)
	require.NoError(t, err)
	rcA, err := r.newRunContext(ctx, run, map[string]any{"v": "a"})
	require.NoError(t, err)
	rcB, err := r.newRunContext(ctx, run, map[string]any{"v": "b"})
	require.NoError(t, err)
	rcEmpty, err := r.newRunContext(ctx, run, map[string]any{"v": ""})
	require.NoError(t, err)
	require.Empty(t, job.Outputs)

	require.NoError(t, rcA.interpolateOutputs()(ctx))
	require.NoError(t, rcB.interpolateOutputs()(ctx))
	require.NoError(t, rcEmpty.interpolateOutputs()(ctx))

	require.Equal(t, map[string]string{"o": "b"}, job.Outputs)
	require.Equal(t, map[string]string{"o": "from-gitea"}, needed.Outputs)
}

func TestInterpolateOutputsFromExpression(t *testing.T) {
	var rawOutputs yaml.Node
	require.NoError(t, rawOutputs.Encode(`${{ fromJSON('{"o":"resolved"}') }}`))

	job := &model.Job{RawOutputs: rawOutputs}
	run := &model.Run{JobID: "j", Workflow: &model.Workflow{Name: "w", Jobs: map[string]*model.Job{"j": job}}}
	rc, err := (&runnerImpl{config: &Config{PresetGitHubContext: &model.GithubContext{}}}).newRunContext(t.Context(), run, nil)
	require.NoError(t, err)

	require.NoError(t, rc.interpolateOutputs()(t.Context()))
	require.Equal(t, "resolved", job.Outputs["o"])
}

func TestGetGitHubContext(t *testing.T) {
	logging.SetLevel(logging.LevelDebug)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	rc := &RunContext{
		Config: &Config{
			EventName: "push",
			Workdir:   cwd,
			Env: map[string]string{
				"GITHUB_REPOSITORY": "nektos/act",
			},
		},
		Run: &model.Run{
			Workflow: &model.Workflow{
				Name: "GitHubContextTest",
				Jobs: map[string]*model.Job{"job1": {}},
			},
		},
		Name:        "GitHubContextTest",
		CurrentStep: "step",
		Matrix:      map[string]any{},
		Env:         map[string]string{},
		ExtraPath:   []string{},
		StepResults: map[string]*model.StepResult{},
	}
	rc.Run.JobID = "job1"

	ghc := rc.getGithubContext(t.Context())

	actor := "nektos/act"
	if a := os.Getenv("ACT_ACTOR"); a != "" {
		actor = a
	}

	repo := "nektos/act"
	if r := os.Getenv("ACT_REPOSITORY"); r != "" {
		repo = r
	}

	owner := "nektos"
	if o := os.Getenv("ACT_OWNER"); o != "" {
		owner = o
	}

	assert.Equal(t, "1", ghc.RunID)
	assert.Equal(t, "1", ghc.RunNumber)
	assert.Equal(t, "0", ghc.RetentionDays)
	assert.Equal(t, actor, ghc.Actor)
	assert.Equal(t, repo, ghc.Repository)
	assert.Equal(t, owner, ghc.RepositoryOwner)
	assert.Equal(t, "/dev/null", ghc.RunnerPerflog)
	assert.Equal(t, rc.Config.Secrets["GITHUB_TOKEN"], ghc.Token)
	assert.Equal(t, "job1", ghc.Job)

	rc.Config.PresetGitHubContext = &model.GithubContext{
		Actor:             "preset-actor",
		ActorID:           "12",
		TriggeringActor:   "triggerer",
		Job:               "preset-job",
		RepositoryID:      "34",
		RepositoryOwnerID: "56",
		WorkflowRef:       "owner/repo/.gitea/workflows/test.yml@refs/heads/main",
		WorkflowSha:       "workflow-sha",
		RefProtected:      true,
	}
	ghc = rc.getGithubContext(t.Context())
	assert.Equal(t, "preset-actor", ghc.Actor)
	assert.Equal(t, "triggerer", ghc.TriggeringActor)
	assert.Equal(t, "job1", ghc.Job)
	env := map[string]string{}
	rc.withGithubEnv(t.Context(), ghc, env)
	for name, value := range map[string]string{
		"GITHUB_ACTOR_ID":            "12",
		"GITHUB_TRIGGERING_ACTOR":    "triggerer",
		"GITHUB_REPOSITORY_ID":       "34",
		"GITHUB_REPOSITORY_OWNER_ID": "56",
		"GITHUB_WORKFLOW_REF":        "owner/repo/.gitea/workflows/test.yml@refs/heads/main",
		"GITHUB_WORKFLOW_SHA":        "workflow-sha",
		"GITHUB_REF_PROTECTED":       "true",
	} {
		assert.Equal(t, value, env[name], name)
	}
	ghc.RefProtected = false
	rc.withGithubEnv(t.Context(), ghc, env)
	assert.Equal(t, "false", env["GITHUB_REF_PROTECTED"])

	preset := &model.GithubContext{ServerURL: "https://preset.test", APIURL: "https://preset.test/api/v1"}
	for _, testcase := range []struct {
		name, instance, serverURL, apiURL string
		preset                            *model.GithubContext
		env                               map[string]string
	}{
		{name: "unknown instance"},
		{name: "bare host with trailing slash", instance: "gitea.test/", serverURL: "https://gitea.test", apiURL: "https://gitea.test/api/v1"},
		{name: "scheme port and base path", instance: "http://gitea.test:3000/gitea/", serverURL: "http://gitea.test:3000/gitea", apiURL: "http://gitea.test:3000/gitea/api/v1"},
		{name: "daemon instance replaces preset endpoints", instance: "gitea.test", preset: preset, serverURL: "https://gitea.test", apiURL: "https://gitea.test/api/v1"},
		{name: "daemon keeps preset endpoints without instance", preset: preset, serverURL: "https://preset.test", apiURL: "https://preset.test/api/v1"},
		{name: "environment overrides", env: map[string]string{"GITHUB_SERVER_URL": "https://override.test", "GITHUB_API_URL": "https://override.test/api"}, serverURL: "https://override.test", apiURL: "https://override.test/api"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			rc.Config.GitHubInstance = testcase.instance
			rc.Config.PresetGitHubContext = testcase.preset
			rc.Config.Env = map[string]string{"GITHUB_REPOSITORY": "owner/repo", "GITHUB_REF": "refs/heads/main", "SHA_REF": "1234567890"}
			maps.Copy(rc.Config.Env, testcase.env)
			github := rc.getGithubContext(t.Context())
			assert.Equal(t, testcase.serverURL, github.ServerURL)
			assert.Equal(t, testcase.apiURL, github.APIURL)
			env := map[string]string{}
			rc.withGithubEnv(t.Context(), github, env)
			_, exported := env["GITHUB_SERVER_URL"]
			assert.Equal(t, testcase.serverURL != "", exported)
			assert.Equal(t, testcase.serverURL, env["GITHUB_SERVER_URL"])
			assert.Equal(t, testcase.apiURL, env["GITHUB_API_URL"])
		})
	}
}

func TestGetGithubContextRef(t *testing.T) {
	table := []struct {
		event string
		json  string
		ref   string
	}{
		{event: "push", json: `{"ref":"0000000000000000000000000000000000000000"}`, ref: "0000000000000000000000000000000000000000"},
		{event: "create", json: `{"ref":"0000000000000000000000000000000000000000"}`, ref: "0000000000000000000000000000000000000000"},
		{event: "workflow_dispatch", json: `{"ref":"0000000000000000000000000000000000000000"}`, ref: "0000000000000000000000000000000000000000"},
		{event: "delete", json: `{"repository":{"default_branch": "main"}}`, ref: "refs/heads/main"},
		{event: "pull_request", json: `{"number":123}`, ref: "refs/pull/123/merge"},
		{event: "pull_request_review", json: `{"number":123}`, ref: "refs/pull/123/merge"},
		{event: "pull_request_review_comment", json: `{"number":123}`, ref: "refs/pull/123/merge"},
		{event: "pull_request_target", json: `{"pull_request":{"base":{"ref": "main"}}}`, ref: "refs/heads/main"},
		{event: "deployment", json: `{"deployment": {"ref": "tag-name"}}`, ref: "tag-name"},
		{event: "deployment_status", json: `{"deployment": {"ref": "tag-name"}}`, ref: "tag-name"},
		{event: "release", json: `{"release": {"tag_name": "tag-name"}}`, ref: "refs/tags/tag-name"},
	}

	for _, data := range table {
		t.Run(data.event, func(t *testing.T) {
			rc := &RunContext{
				EventJSON: data.json,
				Config: &Config{
					EventName: data.event,
					Workdir:   "",
					Env:       map[string]string{"GITHUB_REPOSITORY": "owner/repo", "SHA_REF": "1234567890"},
				},
				Run: &model.Run{
					Workflow: &model.Workflow{
						Name: "GitHubContextTest",
					},
				},
			}

			ghc := rc.getGithubContext(t.Context())

			assert.Equal(t, data.ref, ghc.Ref)
		})
	}
}

func createIfTestRunContext(jobs map[string]*model.Job) *RunContext {
	rc := &RunContext{
		Config: &Config{Workdir: ".", PresetGitHubContext: &model.GithubContext{}, PlatformPicker: func([]string) string { return "ubuntu-latest" }},
		Run: &model.Run{
			JobID: "job1",
			Workflow: &model.Workflow{
				Name: "test-workflow",
				Jobs: jobs,
			},
		},
	}
	rc.ExprEval = rc.NewExpressionEvaluator(context.Background())

	return rc
}

func createJob(t *testing.T, input, result string) *model.Job {
	var job *model.Job
	err := yaml.Unmarshal([]byte(input), &job)
	assert.NoError(t, err)
	job.Result = result

	return job
}

func TestRunContextRunsOnPlatformNames(t *testing.T) {
	for _, testcase := range []struct {
		job  string
		want []string
	}{
		{"runs-on: ubuntu-latest", []string{"ubuntu-latest"}},
		{"runs-on: ${{ 'ubuntu-latest' }}", []string{"ubuntu-latest"}},
		{"runs-on: [self-hosted, my-runner]", []string{"self-hosted", "my-runner"}},
		{`runs-on: [self-hosted, "${{ 'my-runner' }}"]`, []string{"self-hosted", "my-runner"}},
		{`runs-on: ${{ fromJSON('["ubuntu-latest"]') }}`, []string{"ubuntu-latest"}},
		{"name: something", []string{}},
		{"runs-on: {mapping: value}", []string{}},
		{"runs-on: ${{ invalid expression }}", []string{}},
	} {
		t.Run(testcase.job, func(t *testing.T) {
			rc := createIfTestRunContext(map[string]*model.Job{"job1": createJob(t, testcase.job, "")})
			assert.Equal(t, testcase.want, rc.runsOnPlatformNames(t.Context()))
		})
	}
}

func TestRunContextIsEnabled(t *testing.T) {
	for _, testcase := range []struct {
		name, job, neededResult string
		want, envError          bool
	}{
		{name: "broken env fails at setup", job: "runs-on: ubuntu-latest\nenv:\n  BROKEN: ${{ fromJSON('invalid') }}\nif: success()", want: true, envError: true},
		{name: "success with failed dependency", job: "runs-on: ubuntu-latest\nneeds: [needed]\nif: success()", neededResult: "failure"},
		{name: "success with successful dependency", job: "runs-on: ubuntu-latest\nneeds: [needed]\nif: success()", neededResult: "success", want: true},
		{name: "success with unrelated failed job", job: "runs-on: ubuntu-latest\nif: success()", neededResult: "failure", want: true},
		{name: "failure without dependencies", job: "runs-on: ubuntu-latest\nif: failure()"},
		{name: "failure with failed dependency", job: "runs-on: ubuntu-latest\nneeds: [needed]\nif: failure()", neededResult: "failure", want: true},
		{name: "failure with successful dependency", job: "runs-on: ubuntu-latest\nneeds: [needed]\nif: failure()", neededResult: "success"},
		{name: "failure with unrelated failed job", job: "runs-on: ubuntu-latest\nif: failure()", neededResult: "failure"},
		{name: "always without dependencies", job: "runs-on: ubuntu-latest\nif: always()", want: true},
		{name: "always with failed dependency", job: "runs-on: ubuntu-latest\nneeds: [needed]\nif: always()", neededResult: "failure", want: true},
		{name: "always with successful dependency", job: "runs-on: ubuntu-latest\nneeds: [needed]\nif: always()", neededResult: "success", want: true},
		{name: "always with unrelated successful job", job: "runs-on: ubuntu-latest\nif: always()", neededResult: "success", want: true},
		{name: "reusable workflow", job: "uses: ./.github/workflows/reusable.yml", want: true},
		{name: "disabled reusable workflow", job: "uses: ./.github/workflows/reusable.yml\nif: false"},
		{name: "invalid condition", job: "if: ${{ fromJSON('not-json') }}"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			jobs := map[string]*model.Job{"job1": createJob(t, testcase.job, "")}
			if testcase.neededResult != "" {
				jobs["needed"] = createJob(t, "runs-on: ubuntu-latest", testcase.neededResult)
			}
			rc := createIfTestRunContext(jobs)
			enabled, err := rc.isEnabled(t.Context())
			require.NoError(t, err)
			assert.Equal(t, testcase.want, enabled)
			if testcase.envError {
				require.Error(t, evaluateJobEnvAndDefaults(t.Context(), rc))
			}
		})
	}
}

func TestRunContextGetEnv(t *testing.T) {
	for _, testcase := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"Env from Config should overwrite", map[string]string{"OVERWRITTEN": "true"}, "true"},
		{"No overwrite occurs", map[string]string{"SOME_OTHER_VAR": "true"}, "false"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			rc := &RunContext{
				Config: &Config{Env: testcase.env},
				Run: &model.Run{
					JobID: "test",
					Workflow: &model.Workflow{
						Jobs: map[string]*model.Job{"test": {}},
						Env:  map[string]string{"OVERWRITTEN": "false"},
					},
				},
			}
			assert.Equal(t, testcase.want, rc.GetEnv()["OVERWRITTEN"])
		})
	}
}

func TestSetActionEnvRefreshesInputs(t *testing.T) {
	rc := &RunContext{}
	rc.setActionEnv(map[string]string{"INPUT_MSG": "pre"})
	rc.setActionEnv(map[string]string{"INPUT_MSG": "main"})
	assert.Equal(t, map[string]any{"msg": "main"}, rc.actionInputs)
}

func TestCreateContainerNameBoundedForLongMatrixInput(t *testing.T) {
	longMatrixValue := strings.Repeat("os=ubuntu-latest-go=1.24-node=22-", 20)
	name := createContainerName(
		"gitea",
		"WORKFLOW-super-long-workflow-name",
		"JOB-build-matrix-"+longMatrixValue,
	)

	assert.LessOrEqual(t, len(name), 128)
	assert.LessOrEqual(t, len(name+"-env"), 255)
	assert.LessOrEqual(t, len(name+"-network"), 255)
	assert.LessOrEqual(t, len(name+"-job1234567890"), 255)
}

func TestPrintStartJobContainerGroupGolden(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := logging.New(logging.NewLevelHandler(newJobLogHandler(buf, cyan), logging.LevelInfo))
	entry := logger.With("job", "j1")
	ctx := common.WithLogger(t.Context(), entry)

	printStartJobContainerGroup(ctx, "node:20", "GITEA-WORKFLOW-build-JOB-test", "gitea-runner-network")()

	want := strings.Join([]string{
		"[j1]   | ::group::Starting job container",
		"[j1]   | image: node:20",
		"[j1]   | name: GITEA-WORKFLOW-build-JOB-test",
		"[j1]   | network: gitea-runner-network",
		"[j1]   | ::endgroup::",
		"",
	}, "\n")
	assert.Equal(t, want, buf.String())
}

func TestRunContext_cleanupFailedStart(t *testing.T) {
	type ctxKey string
	const sentinel = ctxKey("sentinel")

	t.Run("removes host directories when tool cache creation fails", func(t *testing.T) {
		cacheDir := t.TempDir()
		toolCache := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(toolCache, nil, 0o600))
		rc := &RunContext{
			Config:        &Config{ActionCacheDir: cacheDir},
			Env:           map[string]string{"RUNNER_TOOL_CACHE": toolCache},
			platformImage: "-self-hosted",
		}
		require.Error(t, rc.startContainer()(t.Context()))
		entries, err := os.ReadDir(cacheDir)
		require.NoError(t, err)
		assert.Empty(t, entries)
		assert.FileExists(t, toolCache)
	})

	for name, canceled := range map[string]bool{"cancellation during cleanup": false, "already canceled": true} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), sentinel, "v"))
			defer cancel()
			if canceled {
				cancel()
			}
			calls := 0
			(&RunContext{cleanUpJobContainer: func(ctx context.Context) error {
				calls++
				cancel()
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				assert.WithinDuration(t, time.Now().Add(time.Minute), deadline, time.Second)
				require.NoError(t, ctx.Err())
				assert.Equal(t, "v", ctx.Value(sentinel))
				return nil
			}}).cleanupFailedStart(ctx)
			assert.Equal(t, 1, calls)
		})
	}

	for _, testcase := range []struct {
		name, health          string
		pullError, startError error
	}{
		{name: "healthy", health: container.HealthHealthy},
		{name: "unhealthy", health: container.HealthUnhealthy},
		{name: "pull failure", pullError: errors.New("pull failed")},
		{name: "start failure", startError: errors.New("start failed")},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			var operations []string
			var operationsMu sync.Mutex
			record := func(operation string, failure error) func(context.Context) error {
				return func(context.Context) error {
					operationsMu.Lock()
					defer operationsMu.Unlock()
					operations = append(operations, operation)
					return failure
				}
			}
			job, service := &containerMock{}, &containerMock{}
			for name, instance := range map[string]*containerMock{"job": job, "service": service} {
				instance.On("Pull", true).Return(record(name+".Pull", map[string]error{"job": testcase.pullError}[name]))
				instance.On("Create", mock.Anything, mock.Anything).Return(record(name+".Create", nil))
				instance.On("Start", false).Return(record(name+".Start", map[string]error{"service": testcase.startError}[name]))
				instance.On("Remove").Return(record(name+".Remove", nil))
				instance.On("Close").Return(record(name+".Close", nil))
			}
			job.On("Copy", mock.Anything, mock.Anything).Return(record("job.Copy", nil))
			job.On("Inspect", mock.Anything).Return(&container.Info{ID: "job-id"}, nil).
				Run(func(mock.Arguments) { operations = append(operations, "job.Inspect") })
			for _, health := range []string{container.HealthStarting, testcase.health} {
				service.On("Inspect", mock.Anything).Return(&container.Info{State: "running", Health: health}, nil).
					Run(func(mock.Arguments) { operations = append(operations, "service.Inspect") }).Once()
			}
			stdouts := map[string]io.Writer{}
			service.On("DumpLogs", mock.Anything).Return(nil).Run(func(mock.Arguments) {
				operations = append(operations, "service.DumpLogs")
				fmt.Fprintln(stdouts["postgres:latest"], "::add-mask::leak")
			})
			origNewContainer := newContainer
			newContainer = func(input *container.NewContainerInput) container.ExecutionsEnvironment {
				stdouts[input.Image] = input.Stdout
				return map[string]*containerMock{"postgres:latest": service, "node:20": job}[input.Image]
			}
			t.Cleanup(func() { newContainer = origNewContainer })
			workflow, err := model.ReadWorkflow(strings.NewReader("jobs: {job: {services: {postgres: {image: postgres:latest}}}}"))
			require.NoError(t, err)
			rc := &RunContext{
				Config:        &Config{ForcePull: true, ContainerNetworkMode: "host", Workdir: "/workspace", Env: testCheckoutEnv()},
				Run:           &model.Run{JobID: "job", Workflow: workflow},
				platformImage: "node:20",
			}
			logger, hook := logtest.NewLogger()
			ctx := common.WithLogger(common.WithDryrun(t.Context(), true), logger)
			rc.ExprEval = rc.NewExpressionEvaluator(ctx)
			err = rc.startContainer().Then(rc.stopContainer()).Finally(rc.closeContainer())(ctx)
			want := "job.Remove service.Close service.Remove service.Pull job.Pull"
			switch {
			case testcase.pullError != nil:
				require.ErrorIs(t, err, testcase.pullError)
			case testcase.startError != nil:
				require.ErrorIs(t, err, testcase.startError)
				want += " service.Create service.Start service.DumpLogs"
			default:
				want += " service.Create service.Start service.Inspect job.Create job.Start job.Inspect job.Copy service.Inspect service.DumpLogs"
				if testcase.health == container.HealthUnhealthy {
					require.ErrorContains(t, err, "the service 'postgres' is unhealthy")
				} else {
					require.NoError(t, err)
				}
			}
			slices.Sort(operations[:3])
			slices.Sort(operations[len(operations)-4 : len(operations)-1])
			assert.Equal(t, strings.Fields(want+" job.Remove service.Close service.Remove job.Close"), operations)
			assert.Equal(t, strings.Contains(want, "service.DumpLogs"), slices.Contains(rc.Masks, "leak"))
			assert.Equal(t, testcase.pullError == nil && testcase.health != container.HealthUnhealthy, slices.ContainsFunc(hook.AllRecords(), func(entry logging.Record) bool {
				return strings.HasPrefix(entry.Message, "::group::Print service container logs")
			}))
		})
	}

	(&RunContext{}).cleanupFailedStart(t.Context())
}

func TestWaitForServiceContainers(t *testing.T) {
	newRunContext := func(timeout time.Duration, services ...*serviceContainer) *RunContext {
		return &RunContext{
			Config:            &Config{ServiceReadyTimeout: timeout},
			serviceContainers: services,
		}
	}

	for _, testcase := range []struct {
		name, serviceName string
		timeout           time.Duration
		info              *container.Info
		inspectError      error
		wantError, wantID string
	}{
		{name: "returns as soon as a service without a healthcheck runs", serviceName: "redis", info: &container.Info{ID: "id", State: "running", Health: container.HealthNone}},
		{name: "fails with the probe output when a service is unhealthy", serviceName: "postgres", info: &container.Info{State: "running", Health: container.HealthUnhealthy, HealthOutput: "connection refused"}, wantError: "the service 'postgres' is unhealthy: connection refused"},
		{name: "lets the steps run when a service exits without a healthcheck", serviceName: "postgres", info: &container.Info{State: "exited", ExitCode: 2, Health: container.HealthNone}},
		{name: "proceeds when the container is gone", serviceName: "postgres", inspectError: container.ErrContainerNotFound},
		{name: "does not wait when the timeout is negative", serviceName: "postgres", timeout: -1, info: &container.Info{ID: "id", State: "running", Health: container.HealthStarting}, wantID: "id"},
		{name: "fails on an inspect error even when the timeout is negative", serviceName: "postgres", timeout: -1, inspectError: errors.New("daemon is gone"), wantError: "failed to inspect service 'postgres'"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			service := &containerMock{}
			service.On("Inspect", mock.Anything).Return(testcase.info, testcase.inspectError).Once()
			if testcase.info != nil && testcase.info.Health == container.HealthUnhealthy {
				service.On("DumpLogs", mock.Anything).Return(nil).Once()
			}
			svc := &serviceContainer{name: testcase.serviceName, container: service}
			err := newRunContext(testcase.timeout, svc).waitForServiceContainers()(t.Context())
			if testcase.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), testcase.wantError)
			} else {
				require.NoError(t, err)
			}
			if testcase.wantID != "" {
				assert.Equal(t, testcase.wantID, svc.info.ID)
			}
			service.AssertExpectations(t)
		})
	}

	t.Run("polls at a fixed interval and logs starting only once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var pollTimes []time.Duration
			started := time.Now()
			service := &containerMock{}
			service.On("Inspect", mock.Anything).
				Run(func(_ mock.Arguments) { pollTimes = append(pollTimes, time.Since(started)) }).
				Return(&container.Info{ID: "id", State: "running", Health: container.HealthStarting}, nil).Twice()
			service.On("Inspect", mock.Anything).
				Run(func(_ mock.Arguments) { pollTimes = append(pollTimes, time.Since(started)) }).
				Return(&container.Info{ID: "id", State: "running", Health: container.HealthHealthy}, nil).Once()

			var output bytes.Buffer
			logger := logging.New(logging.NewLevelHandler(newJobLogHandler(&output, cyan), logging.LevelInfo))
			require.NoError(t, newRunContext(0, &serviceContainer{name: "postgres", container: service}).waitForServiceContainers()(common.WithLogger(t.Context(), logger)))
			assert.Equal(t, []time.Duration{0, serviceReadyPollInterval, 2 * serviceReadyPollInterval}, pollTimes)
			assert.Equal(t, 1, strings.Count(output.String(), "postgres service is starting."))
			assert.Contains(t, output.String(), "postgres service is healthy.")
		})
	})

	t.Run("fails right away when one service fails while another is still starting", func(t *testing.T) {
		failing := &containerMock{}
		failing.On("Inspect", mock.Anything).
			Return(&container.Info{State: "running", Health: container.HealthUnhealthy}, nil)
		failing.On("DumpLogs", mock.Anything).Return(nil).Once()
		starting := &containerMock{}
		starting.On("Inspect", mock.Anything).
			Return(&container.Info{State: "running", Health: container.HealthStarting}, nil)

		rc := newRunContext(10*time.Second,
			&serviceContainer{name: "failing", container: failing},
			&serviceContainer{name: "starting", container: starting})

		done := make(chan error, 1)
		go func() { done <- rc.waitForServiceContainers()(context.Background()) }()

		select {
		case err := <-done:
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the service 'failing' is unhealthy")
		case <-time.After(2 * time.Second):
			t.Fatal("waitForServiceContainers did not fail fast; it waited for the starting service")
		}
	})

	t.Run("gives up once the timeout expires", func(t *testing.T) {
		service := &containerMock{}
		service.On("Inspect", mock.Anything).
			Return(&container.Info{State: "running", Health: container.HealthStarting}, nil)

		rc := newRunContext(20*time.Millisecond, &serviceContainer{name: "postgres", container: service})
		err := rc.waitForServiceContainers()(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not become healthy within")
	})

	t.Run("gives up with the same message when the deadline stops an inspect", func(t *testing.T) {
		service := &containerMock{}
		service.On("Inspect", mock.Anything).
			Run(func(args mock.Arguments) { <-args.Get(0).(context.Context).Done() }).
			Return((*container.Info)(nil), errors.New("inspect aborted"))

		rc := newRunContext(20*time.Millisecond, &serviceContainer{name: "postgres", container: service})
		err := rc.waitForServiceContainers()(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not become healthy within")
	})

	t.Run("is a no-op without services", func(t *testing.T) {
		require.NoError(t, newRunContext(0).waitForServiceContainers()(t.Context()))
	})
}

func TestReportUnstartedServices(t *testing.T) {
	dead := &containerMock{}
	dead.On("Inspect", mock.Anything).Return(&container.Info{ID: "dead-id", State: "exited", ExitCode: 1}, nil).Once()
	dead.On("DumpLogs", mock.Anything).Return(nil).Once()
	running := &containerMock{}
	running.On("Inspect", mock.Anything).Return(&container.Info{ID: "run-id", State: "running"}, nil).Once()

	rc := &RunContext{serviceContainers: []*serviceContainer{
		{name: "postgres", container: dead},
		{name: "redis", container: running},
	}}

	require.NoError(t, rc.reportUnstartedServices()(t.Context()))
	dead.AssertExpectations(t)
	running.AssertExpectations(t)
}

func TestGetJobContextReportsContainers(t *testing.T) {
	rc := &RunContext{
		jobNetworkName: "job-network",
		jobContainerID: "job-container-id",
		serviceContainers: []*serviceContainer{
			{name: "postgres", info: &container.Info{ID: "svc-id", Ports: map[string]string{"5432": "49153"}}},
			{name: "redis", info: &container.Info{ID: "redis-id", Ports: map[string]string{}}},
			{name: "mailhog"},
		},
	}

	jobContext := rc.getJobContext()

	assert.Equal(t, "job-container-id", jobContext.Container.ID)
	assert.Equal(t, "job-network", jobContext.Container.Network)
	assert.Equal(t, map[string]model.JobService{
		"postgres": {ID: "svc-id", Network: "job-network", Ports: map[string]string{"5432": "49153"}},
		"redis":    {ID: "redis-id", Network: "job-network", Ports: map[string]string{}},
	}, jobContext.Services)
}

func TestCaptureJobContainerInfoExportsDockerWorkspace(t *testing.T) {
	job := &containerMock{}
	job.On("Inspect", mock.Anything).Return(&container.Info{
		ID:     "job-container-id",
		Mounts: map[string]string{"/workspace": "/var/lib/docker/volumes/job/_data"},
	}, nil)
	rc := &RunContext{
		Config:       &Config{Workdir: "/workspace/owner/repo/"},
		Env:          map[string]string{},
		JobContainer: job,
	}

	require.NoError(t, rc.captureJobContainerInfo()(t.Context()))

	assert.Equal(t, "job-container-id", rc.jobContainerID)
	assert.Equal(t, "/var/lib/docker/volumes/job/_data/owner/repo", rc.Env["GITEA_DOCKER_WORKSPACE"])

	rc.Config.Workdir = "/elsewhere"
	rc.Env = map[string]string{}
	require.NoError(t, rc.captureJobContainerInfo()(t.Context()))
	assert.NotContains(t, rc.Env, "GITEA_DOCKER_WORKSPACE")
}

func TestGetJobContextWithoutContainer(t *testing.T) {
	jobContext := (&RunContext{}).getJobContext()

	assert.Empty(t, jobContext.Container.ID)
	assert.Empty(t, jobContext.Container.Network)
	assert.Empty(t, jobContext.Services)
}

func TestImageOSFromImage(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  string
	}{
		{"", ""},
		{"docker.gitea.com/runner-images:ubuntu-24.04", "ubuntu24"},
		{"docker.gitea.com/runner-images:ubuntu-latest", ""},
		{"runner-images:ubuntu22.04", "ubuntu22"},
		{"node:20", ""},
		{"ubuntu:22.04", ""},
		{"ubuntu", ""},
		{"catthehacker/ubuntu:act-22.04", ""},
		{"myco/ubuntu:v2.1", ""},
		{"myco/ubuntu:v22.04", ""},
		{"app:release-1", ""},
		{"app:1.2.3", ""},
		{"app:build-2.1", ""},
		{"registry.example.com:5000/runner-images", ""},
		{"registry.example.com:5000/runner-images:ubuntu-24.04", "ubuntu24"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			assert.Equal(t, tc.want, imageOSFromImage(tc.image))
		})
	}
}

func TestResolvePlatformImageAppliesImagelessContainerToRunsOnImage(t *testing.T) {
	for _, tc := range []struct {
		name, image string
		want        model.ContainerSpec
	}{
		{"without image key", "", model.ContainerSpec{Volumes: []string{"/host/data:/data"}, Env: map[string]string{"KEY": "value"}}},
		{"with empty image", "image: '', ", model.ContainerSpec{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := createIfTestRunContext(map[string]*model.Job{
				"job1": createJob(t, "runs-on: toolkit\ncontainer: {"+tc.image+"volumes: [/host/data:/data], env: {KEY: value}}", ""),
			})
			require.NoError(t, rc.resolvePlatformImage(t.Context()))
			require.NoError(t, evaluateJobEnvAndDefaults(t.Context(), rc))

			assert.Equal(t, "ubuntu-latest", rc.platformImage)
			assert.Equal(t, tc.want, rc.containerSpec)
		})
	}
}

func createRunsOnRunContext(t *testing.T, runsOn string) *RunContext {
	return createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, "runs-on: "+runsOn, ""),
	})
}

func TestRunContextContainerImageCannotSelectBackend(t *testing.T) {
	rc := createIfTestRunContext(map[string]*model.Job{"job1": createJob(t, "container: -self-hosted", "")})
	require.NoError(t, rc.resolvePlatformImage(t.Context()))
	assert.False(t, rc.IsHostEnv())

	rc = createIfTestRunContext(map[string]*model.Job{"job1": createJob(t, "runs-on: ${{ fromJSON('x') }}\ncontainer: node", "")})
	require.ErrorContains(t, rc.resolvePlatformImage(t.Context()), "runs-on")
}

func TestRunContextImageOS(t *testing.T) {
	ctx := t.Context()

	t.Run("prefers the release in the resolved image tag", func(t *testing.T) {
		rc := createRunsOnRunContext(t, "ubuntu-latest")
		rc.Config.PlatformPicker = func([]string) string { return "docker.gitea.com/runner-images:ubuntu-24.04" }
		require.NoError(t, rc.resolvePlatformImage(ctx))
		assert.Equal(t, "ubuntu24", rc.imageOS(ctx))
	})

	t.Run("falls back to the runs-on label", func(t *testing.T) {
		rc := createRunsOnRunContext(t, "ubuntu-22.04")
		rc.Config.PlatformPicker = func([]string) string { return "some-image" }
		require.NoError(t, rc.resolvePlatformImage(ctx))
		assert.Equal(t, "ubuntu22", rc.imageOS(ctx))
	})

	t.Run("keeps the historical value for a rolling label with no release", func(t *testing.T) {
		assert.Equal(t, "ubuntu20", createRunsOnRunContext(t, "ubuntu-latest").imageOS(ctx))
	})

	t.Run("is empty for the synthetic job of a composite action", func(t *testing.T) {
		rc := createIfTestRunContext(map[string]*model.Job{"job1": {}})
		assert.Empty(t, rc.imageOS(ctx))
	})
}

func TestRunContextGetRunnerContext(t *testing.T) {
	ctx := t.Context()

	t.Run("adds the runner values the container cannot know", func(t *testing.T) {
		rc := createRunsOnRunContext(t, "ubuntu-latest")
		rc.Config.RunnerName = "runner-1"
		rc.Config.Workdir = "/workspace/owner/repo"

		runnerContext := rc.getRunnerContext(ctx)
		assert.Equal(t, "runner-1", runnerContext["name"])
		assert.Equal(t, "self-hosted", runnerContext["environment"])
		assert.Equal(t, "/workspace/owner", runnerContext["workspace"])
		assert.Equal(t, "/workspace/owner/repo", rc.getGithubContext(ctx).Workspace)
		rc.Config.Env = map[string]string{"GITHUB_WORKSPACE": "/configured/work"}
		assert.Equal(t, "/configured/work", rc.getGithubContext(ctx).Workspace)
		assert.NotContains(t, runnerContext, "debug")
	})

	t.Run("reports debug when step debugging is on", func(t *testing.T) {
		rc := createRunsOnRunContext(t, "ubuntu-latest")
		rc.Config.Secrets = map[string]string{"ACTIONS_STEP_DEBUG": "true"}

		assert.Equal(t, "1", rc.getRunnerContext(ctx)["debug"])
	})

	t.Run("keeps the execution environment values", func(t *testing.T) {
		rc := createRunsOnRunContext(t, "ubuntu-latest")
		rc.Config.Workdir = "/host/work/owner/repo"
		rc.JobContainer = &container.HostEnvironment{Workdir: rc.Config.Workdir, Path: "/container/work/owner/repo", TmpDir: "/tmp/act", ToolCache: "/tmp/tool_cache"}

		runnerContext := rc.getRunnerContext(ctx)
		assert.Equal(t, "/tmp/act", runnerContext["temp"])
		assert.Equal(t, "/tmp/tool_cache", runnerContext["tool_cache"])
		assert.Equal(t, "/container/work/owner", runnerContext["workspace"])
		assert.Equal(t, "/container/work/owner/repo", rc.getGithubContext(ctx).Workspace)
		assert.NotEmpty(t, runnerContext["os"])
	})
}

func TestRunContextUpdateExtraPath(t *testing.T) {
	t.Parallel()
	longPath := strings.Repeat("x", 64*1024+1)
	for _, testcase := range []struct {
		name    string
		content []byte
		want    string
		wantErr bool
	}{
		{name: "UTF-8 BOM", content: []byte("\xef\xbb\xbf/utf8/tools\n"), want: "/utf8/tools"},
		{name: "UTF-16 BOM", content: []byte{0xff, 0xfe, 'C', 0, ':', 0, '\\', 0, 't', 0, 'o', 0, 'o', 0, 'l', 0, 's', 0, '\n', 0}, want: `C:\tools`},
		{name: "over 64 KiB", content: []byte(longPath + "\n"), want: longPath},
		{name: "over 16 MiB", content: []byte(strings.Repeat("x", 16*1024*1024+1)), wantErr: true},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			logger := logging.New(logging.DiscardHandler)
			ctx := common.WithLogger(t.Context(), logger)
			jobContainer := &containerMock{}
			jobContainer.On("GetContainerArchive", mock.Anything, "/github/path").
				Return(io.NopCloser(bytes.NewReader(tarArchive(t, tarEntry{name: "path", body: string(testcase.content)}))), nil).Once()
			defer jobContainer.AssertExpectations(t)
			rc := &RunContext{JobContainer: jobContainer}

			err := rc.UpdateExtraPath(ctx, "/github/path")
			if testcase.wantErr {
				require.ErrorContains(t, err, "reading path file")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{testcase.want}, rc.ExtraPath)
		})
	}
}

func TestParentDir(t *testing.T) {
	assert.Empty(t, parentDir(""))
	assert.Empty(t, parentDir("repo"))
	assert.Empty(t, parentDir("/repo"))
	assert.Equal(t, "/workspace/owner", parentDir("/workspace/owner/repo"))
	assert.Equal(t, `C:\workspace\owner`, parentDir(`C:\workspace\owner\repo`))
}

func TestRunContextWithGithubEnvRunnerValues(t *testing.T) {
	ctx := t.Context()
	rc := createRunsOnRunContext(t, "ubuntu-latest")
	rc.Config.RunnerName = "runner-1"
	rc.Config.Secrets = map[string]string{"ACTIONS_STEP_DEBUG": "true"}
	t.Setenv("ACTIONS_RUNTIME_URL", "")
	rc.Config.ArtifactServerPath = "artifacts"
	rc.Config.ArtifactServerAddr = "2001:db8::1"
	rc.Config.ArtifactServerPort = "8080"

	env := map[string]string{}
	rc.withGithubEnv(ctx, &model.GithubContext{Workspace: "/workspace/owner/repo"}, env)

	assert.Equal(t, "runner-1", env["RUNNER_NAME"])
	assert.Equal(t, "self-hosted", env["RUNNER_ENVIRONMENT"])
	assert.Equal(t, "/workspace/owner", env["RUNNER_WORKSPACE"])
	assert.Equal(t, "1", env["RUNNER_DEBUG"])
	assert.Equal(t, "http://[2001:db8::1]:8080/", env["ACTIONS_RUNTIME_URL"])
	assert.Equal(t, "true", env["CI"])

	for _, ci := range []string{"false", ""} {
		env["CI"] = ci
		rc.withGithubEnv(ctx, &model.GithubContext{}, env)
		assert.Equal(t, ci, env["CI"])
	}
}
