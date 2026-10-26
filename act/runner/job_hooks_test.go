// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"testing"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookContainer records the command a hook was run with and answers with what the hook
// wrote to its GITHUB_ENV and GITHUB_PATH files.
type hookContainer struct {
	fakeContainer
	cmd     []string
	env     map[string]string
	err     error
	copyErr error
	envFile map[string]string
	pathTar []byte
	output  string
	stdout  io.Writer
}

func (c *hookContainer) ToContainerPath(path string) string { return path }
func (c *hookContainer) IsEnvironmentCaseInsensitive() bool { return false }

func (c *hookContainer) GetRunnerContext(context.Context) map[string]any {
	return map[string]any{"os": "Linux"}
}

func (c *hookContainer) Exec(command []string, env map[string]string, _, _ string) common.Executor {
	return func(context.Context) error {
		c.cmd, c.env = command, env
		if c.output != "" {
			if _, err := io.WriteString(c.stdout, c.output); err != nil {
				return err
			}
		}
		return c.err
	}
}

func (c *hookContainer) ReplaceLogWriter(stdout, _ io.Writer) (io.Writer, io.Writer) {
	old := c.stdout
	c.stdout = stdout
	return old, nil
}

func (c *hookContainer) Copy(_destPath string, _files ...*container.FileEntry) common.Executor {
	return common.NewErrorExecutor(c.copyErr)
}

func (c *hookContainer) UpdateFromEnv(_ string, env *map[string]string) common.Executor {
	return func(context.Context) error {
		maps.Copy(*env, c.envFile)
		return nil
	}
}

func (c *hookContainer) GetContainerArchive(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(c.pathTar)), nil
}

// newHookRunContext returns a RunContext and the context to run a hook with, whose logger is
// silenced so the hook's job-log output does not reach the test output.
func newHookRunContext(jobContainer *hookContainer, config *Config) (*RunContext, context.Context) {
	config.PresetGitHubContext = &model.GithubContext{}
	rc := &RunContext{
		Config:       config,
		Run:          &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
		JobContainer: jobContainer,
	}
	logger := logging.New(logtest.New())
	ctx := common.WithJobErrorContainer(common.WithLogger(context.Background(), logger.With("test", true)))
	rc.ExprEval = rc.NewExpressionEvaluator(ctx)
	return rc, ctx
}

func TestRunJobHook(t *testing.T) {
	t.Run("runs the hook with the job environment", func(t *testing.T) {
		jobContainer := &hookContainer{}
		rc, ctx := newHookRunContext(jobContainer, &Config{
			JobStartedHook: "/hooks/started.sh",
			Env:            map[string]string{"A_VAR": "value", jobStartedHookEnv: "/from/env.sh"},
		})

		require.NoError(t, rc.runJobStartedHook(ctx))

		// The setting wins over the environment variable.
		assert.Equal(t, []string{"bash", "-e", "/hooks/started.sh"}, jobContainer.cmd)
		assert.Equal(t, "value", jobContainer.env["A_VAR"])
		// The github environment is there too, so a hook can tell which job it runs for.
		assert.Equal(t, "job", jobContainer.env["GITHUB_JOB"])
		assert.Equal(t, "/var/run/act/workflow/hook-envs.txt", jobContainer.env["GITHUB_ENV"])
		assert.Equal(t, "/var/run/act/workflow/hook-path.txt", jobContainer.env["GITHUB_PATH"])
	})

	t.Run("each hook has its own command state", func(t *testing.T) {
		jobContainer := &hookContainer{output: "::stop-commands::token77"}
		rc, ctx := newHookRunContext(jobContainer, &Config{JobStartedHook: "/started.sh", JobCompletedHook: "/completed.sh"})
		jobWriter := rc.commandLogWriter(ctx)
		jobContainer.stdout = jobWriter

		require.NoError(t, rc.runJobStartedHook(ctx))
		jobContainer.output = "::add-mask::s3cret"
		require.NoError(t, rc.runJobCompletedHook(ctx))
		assert.Equal(t, []string{"token77", "s3cret"}, rc.Masks)
		assert.Same(t, jobWriter, jobContainer.stdout)
	})

	t.Run("fails on refused commands and honors the container opt-in", func(t *testing.T) {
		for _, allow := range []bool{false, true} {
			jobContainer := &hookContainer{output: "::set-env name=FROM_HOOK::1\n::add-path::/opt/hook/bin"}
			rc, ctx := newHookRunContext(jobContainer, &Config{JobStartedHook: "/started.sh"})
			if allow {
				rc.containerSpec.Env = map[string]string{allowUnsecureCommandsVar: "true"}
			}

			err := rc.runJobStartedHook(ctx)
			if allow {
				require.NoError(t, err)
				assert.Equal(t, "1", rc.Env["FROM_HOOK"])
				assert.Equal(t, []string{"/opt/hook/bin"}, rc.ExtraPath)
			} else {
				require.ErrorContains(t, err, "workflow command is disabled")
				assert.Equal(t, "failure", rc.getJobContext().Status)
				require.ErrorContains(t, common.JobError(ctx), "workflow command is disabled")
				assert.NotContains(t, rc.Env, "FROM_HOOK")
				assert.Empty(t, rc.ExtraPath)
			}
			assert.Nil(t, rc.currentStepEnv())
			require.NoError(t, rc.takeUnsecureCommandError())
		}
	})

	t.Run("the completed hook has no preceding step identity", func(t *testing.T) {
		jobContainer := &hookContainer{output: "::set-output name=value::after\n::save-state name=value::after"}
		rc, ctx := newHookRunContext(jobContainer, &Config{JobCompletedHook: "/completed.sh"})
		rc.CurrentStep = "last-step"
		rc.StepResults = map[string]*model.StepResult{"last-step": {Outputs: map[string]string{"value": "before"}}}

		require.NoError(t, rc.runJobCompletedHook(ctx))
		assert.Empty(t, jobContainer.env["GITHUB_ACTION"])
		assert.Equal(t, "before", rc.StepResults["last-step"].Outputs["value"])
		assert.Empty(t, rc.IntraActionState)
		assert.Equal(t, "last-step", rc.CurrentStep)
	})

	// Each hook reads its own variable, so a swapped constant cannot pass.
	t.Run("falls back to the GitHub environment variables", func(t *testing.T) {
		for name, hook := range map[string]struct {
			env string
			run func(*RunContext, context.Context) error
		}{
			"started":   {jobStartedHookEnv, (*RunContext).runJobStartedHook},
			"completed": {jobCompletedHookEnv, (*RunContext).runJobCompletedHook},
		} {
			t.Run(name, func(t *testing.T) {
				jobContainer := &hookContainer{}
				rc, ctx := newHookRunContext(jobContainer, &Config{Env: map[string]string{hook.env: "/from/env.sh"}})

				require.NoError(t, hook.run(rc, ctx))
				assert.Equal(t, []string{"bash", "-e", "/from/env.sh"}, jobContainer.cmd)
			})
		}
	})

	t.Run("exports what the hook wrote to GITHUB_ENV and GITHUB_PATH", func(t *testing.T) {
		jobContainer := &hookContainer{
			envFile: map[string]string{"FROM_HOOK": "1"},
			pathTar: tarArchive(t, tarEntry{name: "hook-path.txt", body: "/opt/tool/bin\n"}),
		}
		rc, ctx := newHookRunContext(jobContainer, &Config{JobStartedHook: "/hooks/started.sh"})

		require.NoError(t, rc.runJobStartedHook(ctx))

		assert.Equal(t, "1", rc.Env["FROM_HOOK"])
		assert.Equal(t, []string{"/opt/tool/bin"}, rc.ExtraPath)
	})

	t.Run("a failing hook fails the job", func(t *testing.T) {
		rc, ctx := newHookRunContext(&hookContainer{err: errors.New("boom")}, &Config{JobStartedHook: "/hooks/started.sh"})

		err := rc.runJobStartedHook(ctx)

		require.ErrorContains(t, err, `the job started hook "/hooks/started.sh" failed`)
		require.ErrorContains(t, err, "boom")
		// The failure has to flip the job status, or success()-default steps would still
		// run and the task would be reported successful despite the missing setup.
		assert.Equal(t, "failure", rc.getJobContext().Status)
		require.ErrorContains(t, common.JobError(ctx), "boom")
	})

	t.Run("a hook whose file commands cannot be reset neither runs nor exports stale ones", func(t *testing.T) {
		jobContainer := &hookContainer{copyErr: errors.New("boom"), envFile: map[string]string{"STALE": "1"}}
		rc, ctx := newHookRunContext(jobContainer, &Config{JobStartedHook: "/hooks/started.sh"})

		require.ErrorContains(t, rc.runJobStartedHook(ctx), "boom")
		assert.Nil(t, jobContainer.cmd)
		assert.NotContains(t, rc.Env, "STALE")
	})

	t.Run("is a no-op without a hook", func(t *testing.T) {
		jobContainer := &hookContainer{}
		rc, ctx := newHookRunContext(jobContainer, &Config{})

		require.NoError(t, rc.runJobStartedHook(ctx))
		require.NoError(t, rc.runJobCompletedHook(ctx))
		assert.Nil(t, jobContainer.cmd)
	})
}

// actions/runner deliberately runs a hook without the flags it gives `run:` steps, and an
// executable without a known extension speaks for itself through its shebang.
func TestHookCommand(t *testing.T) {
	for hookPath, want := range map[string]struct {
		cmd   []string
		shell string
	}{
		"/hooks/started.sh":  {[]string{"bash", "-e", "/hooks/started.sh"}, "bash -e {0}"},
		"/hooks/started.PS1": {[]string{"pwsh", "-command", ". '/hooks/started.PS1'"}, `pwsh -command ". '{0}'"`},
		"/hooks/started":     {[]string{"/hooks/started"}, ""},
	} {
		cmd, shell := hookCommand(hookPath)
		assert.Equal(t, want.cmd, cmd, hookPath)
		assert.Equal(t, want.shell, shell, hookPath)
	}
}
