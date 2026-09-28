// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type shellContainerMock struct{ *containerMock }

func (*shellContainerMock) ReplaceLogWriter(_, _ io.Writer) (io.Writer, io.Writer) { return nil, nil }

func TestStepRun(t *testing.T) {
	cm := &containerMock{}
	fileEntry := &container.FileEntry{
		Name: "workflow/1.sh",
		Mode: 0o755,
		Body: "\ncmd\n",
	}

	sr := &stepRun{
		RunContext: &RunContext{
			StepResults: map[string]*model.StepResult{},
			ExprEval:    &expressionEvaluator{},
			Config:      &Config{},
			Run: &model.Run{
				JobID: "1",
				Workflow: &model.Workflow{
					Jobs: map[string]*model.Job{
						"1": {},
					},
				},
			},
			JobContainer:   cm,
			jobRunDefaults: model.RunDefaults{Shell: "bash"},
		},
		Step: &model.Step{
			ID:               "1",
			Run:              "cmd",
			WorkingDirectory: "workdir",
		},
	}

	cm.On("Copy", "/var/run/act", []*container.FileEntry{fileEntry}).Return(noopExecutor)
	cm.On("Exec", []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "/var/run/act/workflow/1.sh"}, mock.AnythingOfType("map[string]string"), "", "workdir").Return(noopExecutor)

	cm.On("Copy", "/var/run/act", mock.AnythingOfType("[]*container.FileEntry")).Return(noopExecutor)

	cm.On("UpdateFromEnv", "/var/run/act/workflow/envs.txt", mock.AnythingOfType("*map[string]string")).Return(noopExecutor)

	cm.On("UpdateFromEnv", "/var/run/act/workflow/statecmd.txt", mock.AnythingOfType("*map[string]string")).Return(noopExecutor)

	cm.On("UpdateFromEnv", "/var/run/act/workflow/outputcmd.txt", mock.AnythingOfType("*map[string]string")).Return(noopExecutor)

	ctx := context.Background()

	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/pathcmd.txt").Return(io.NopCloser(&bytes.Buffer{}), nil)

	err := sr.main()(ctx)
	assert.NoError(t, err) //nolint:testifylint // pre-existing issue from nektos/act

	cm.AssertExpectations(t)
}

func TestStepRunShellParity(t *testing.T) {
	tests := []struct {
		name, run, shell, workingDir string
		env                          map[string]string
		host                         bool
		probeErr                     error
		wantExt                      string
		wantCmd                      []string
		wantErr                      string
	}{
		{
			name:    "implicit host bash",
			host:    true,
			wantCmd: []string{"bash", "-e", "/var/run/act/workflow/1.sh"},
		},
		{
			name:    "implicit container bash",
			wantCmd: []string{"bash", "-e", "/var/run/act/workflow/1.sh"},
		},
		{
			name:     "implicit container sh fallback",
			probeErr: assert.AnError,
			wantCmd:  []string{"sh", "-e", "/var/run/act/workflow/1.sh"},
		},
		{
			name:    "custom pwsh template",
			shell:   "pwsh -NoProfile -File {0}",
			wantExt: ".ps1",
			wantCmd: []string{"pwsh", "-NoProfile", "-File", "/var/run/act/workflow/1.ps1"},
		},
		{
			name:    "missing placeholder",
			shell:   "bash -e",
			wantErr: `invalid shell option "bash -e": format must contain {0}`,
		},
		{
			name:    "all placeholders",
			shell:   "bash -c '. {0}; . {0}'",
			wantCmd: []string{"bash", "-c", ". /var/run/act/workflow/1.sh; . /var/run/act/workflow/1.sh"},
		},
		{
			name:       "step env expressions",
			shell:      "${{ env.SHELL }}",
			workingDir: "${{ env.DIR }}",
			env:        map[string]string{"SHELL": "python {0}", "DIR": "subdir"},
			wantExt:    ".py",
			wantCmd:    []string{"python", "/var/run/act/workflow/1.py"},
		},
		{
			name:    "run expression without a context",
			run:     "echo ${{ COMMIT_SHA }}",
			wantErr: "unable to interpolate the run script:",
		},
		{
			name:    "shell expression without a context",
			shell:   "${{ SHELL }}",
			wantErr: "unable to interpolate the shell:",
		},
		{
			name:       "working directory expression without a context",
			workingDir: "${{ DIR }}",
			wantErr:    "unable to interpolate the working directory:",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cm := &containerMock{}
			var jobContainer container.ExecutionsEnvironment = &shellContainerMock{cm}
			if test.host {
				if runtime.GOOS == "windows" {
					t.Skip("Linux host shell selection")
				}
				test.env = map[string]string{"PATH": t.TempDir()}
				require.NoError(t, os.WriteFile(filepath.Join(test.env["PATH"], "bash"), nil, 0o755))
				jobContainer = &container.HostEnvironment{ActPath: "/var/run/act"}
			} else if test.shell == "" {
				cm.On("Exec", []string{"sh", "-c", "command -v bash >/dev/null 2>&1"},
					mock.AnythingOfType("map[string]string"), "", "").Return(func(context.Context) error {
					return test.probeErr
				})
			}

			sr := &stepRun{
				RunContext: &RunContext{
					Config:       &Config{},
					Run:          &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
					JobContainer: jobContainer,
				},
				Step: &model.Step{ID: "1", Run: cmp.Or(test.run, "echo hi"), Shell: test.shell, WorkingDirectory: test.workingDir},
				env:  test.env,
			}

			name, script, err := sr.setupShellCommand(t.Context())
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			if test.wantExt == "" {
				test.wantExt = ".sh"
			}
			wantScript := "\necho hi\n"
			if test.wantExt == ".ps1" {
				wantScript = "$ErrorActionPreference = 'stop'\necho hi\nif ((Test-Path -LiteralPath variable:/LASTEXITCODE)) { exit $LASTEXITCODE }"
			}
			assert.Equal(t, "workflow/1"+test.wantExt, name)
			assert.Equal(t, wantScript, script)
			assert.Equal(t, test.wantCmd, sr.cmd)
			assert.Equal(t, test.env["DIR"], sr.WorkingDirectory)
			cm.AssertExpectations(t)
		})
	}
}
