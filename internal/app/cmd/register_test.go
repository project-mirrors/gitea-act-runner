// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/stretchr/testify/require"
)

func TestRegisterNonInteractiveReturnsLabelValidationError(t *testing.T) {
	err := registerNoInteractive(t.Context(), "", &registerArgs{
		Labels:       "ubuntu:host,,broken",
		Token:        "token",
		InstanceAddr: "http://localhost:3000",
	})
	require.ErrorContains(t, err, "empty label")
}

func TestRegisterInputsValidate(t *testing.T) {
	tests := []struct {
		name    string
		inputs  registerInputs
		wantErr string
	}{
		{
			name:    "empty instance address",
			inputs:  registerInputs{Token: "token"},
			wantErr: "instance address is empty",
		},
		{
			name:    "empty token",
			inputs:  registerInputs{InstanceAddr: "http://localhost:3000"},
			wantErr: "token is empty",
		},
		{
			name:    "invalid label",
			inputs:  registerInputs{InstanceAddr: "http://localhost:3000", Token: "token", Labels: []string{""}},
			wantErr: "empty label",
		},
		{
			name:   "valid",
			inputs: registerInputs{InstanceAddr: "http://localhost:3000", Token: "token", Labels: []string{"ubuntu:host"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.inputs.validate()
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateLabels(t *testing.T) {
	require.NoError(t, validateLabels([]string{"ubuntu:host", "ubuntu:docker://node:18"}))
	require.NoError(t, validateLabels([]string{"pool:e57e18d4-10d4-406f-93bf-60f127221bdd"}))
	require.Error(t, validateLabels([]string{"ubuntu:host", ""}))
}

func TestRegisterInputsStageValue(t *testing.T) {
	inputs := &registerInputs{
		InstanceAddr: "http://localhost:3000",
		Token:        "token",
		RunnerName:   "runner",
		Labels:       []string{"ubuntu:host", "ubuntu:docker://node:18"},
	}
	require.Equal(t, "http://localhost:3000", inputs.stageValue(StageInputInstance))
	require.Equal(t, "token", inputs.stageValue(StageInputToken))
	require.Equal(t, "runner", inputs.stageValue(StageInputRunnerName))
	require.Equal(t, "ubuntu:host,ubuntu:docker://node:18", inputs.stageValue(StageInputLabels))
	require.Empty(t, (&registerInputs{}).stageValue(StageInputLabels))
	require.Empty(t, inputs.stageValue(StageWaitingForRegistration))
}

func TestRegisterInputsAssignToNext(t *testing.T) {
	emptyCfg := &config.Config{}

	t.Run("instance and token stay on empty value", func(t *testing.T) {
		inputs := &registerInputs{}
		require.Equal(t, StageInputInstance, inputs.assignToNext(StageInputInstance, "", emptyCfg))
		require.Equal(t, StageInputToken, inputs.assignToNext(StageInputToken, "", emptyCfg))
	})

	t.Run("instance then token then runner name", func(t *testing.T) {
		inputs := &registerInputs{}
		require.Equal(t, StageInputToken, inputs.assignToNext(StageInputInstance, "http://localhost:3000", emptyCfg))
		require.Equal(t, "http://localhost:3000", inputs.InstanceAddr)
		require.Equal(t, StageInputRunnerName, inputs.assignToNext(StageInputToken, "token", emptyCfg))
		require.Equal(t, "token", inputs.Token)
	})

	t.Run("empty runner name falls back to hostname", func(t *testing.T) {
		inputs := &registerInputs{}
		require.Equal(t, StageInputLabels, inputs.assignToNext(StageInputRunnerName, "", emptyCfg))
		hostname, _ := os.Hostname()
		require.Equal(t, hostname, inputs.RunnerName)
	})

	t.Run("labels from config skip the labels stage", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Runner.Labels = []string{"ubuntu:host", "", "pool:e57e18d4"}
		inputs := &registerInputs{}
		require.Equal(t, StageWaitingForRegistration, inputs.assignToNext(StageInputRunnerName, "runner", cfg))
		require.Equal(t, []string{"ubuntu:host", "pool:e57e18d4"}, inputs.Labels)
	})

	t.Run("blank labels input uses defaults", func(t *testing.T) {
		inputs := &registerInputs{}
		require.Equal(t, StageWaitingForRegistration, inputs.assignToNext(StageInputLabels, "", emptyCfg))
		require.Equal(t, defaultLabels, inputs.Labels)
	})

	t.Run("invalid labels input loops back", func(t *testing.T) {
		inputs := &registerInputs{}
		require.Equal(t, StageInputLabels, inputs.assignToNext(StageInputLabels, "ubuntu:host,,bad", emptyCfg))
		require.Nil(t, inputs.Labels)
	})

	t.Run("labels containing a colon are accepted", func(t *testing.T) {
		inputs := &registerInputs{}
		require.Equal(t, StageWaitingForRegistration, inputs.assignToNext(StageInputLabels, "pool:e57e18d4,ubuntu:host", emptyCfg))
		require.Equal(t, []string{"pool:e57e18d4", "ubuntu:host"}, inputs.Labels)
	})

	t.Run("overwrite local config", func(t *testing.T) {
		inputs := &registerInputs{}
		require.Equal(t, StageInputInstance, inputs.assignToNext(StageOverwriteLocalConfig, "Y", emptyCfg))
		require.Equal(t, StageInputInstance, inputs.assignToNext(StageOverwriteLocalConfig, "y", emptyCfg))
		require.Equal(t, StageExit, inputs.assignToNext(StageOverwriteLocalConfig, "n", emptyCfg))
	})

	t.Run("unknown stage", func(t *testing.T) {
		inputs := &registerInputs{}
		require.Equal(t, StageUnknown, inputs.assignToNext(StageWaitingForRegistration, "x", emptyCfg))
	})
}

func TestInitInputs(t *testing.T) {
	for _, tc := range []struct {
		name, tokenFile, wantErr string
		env                      *string
	}{
		{name: "missing token", wantErr: "missing token, token-file argument, or GITEA_RUNNER_REGISTRATION_TOKEN environment variable"},
		{name: "empty token", env: new(""), wantErr: "missing token, token-file argument, or GITEA_RUNNER_REGISTRATION_TOKEN environment variable"},
		{
			name: "invalid token file", tokenFile: "/tmp/nonexistent", env: new("from-env"),
			wantErr: "cannot read the token file: /tmp/nonexistent, open /tmp/nonexistent: no such file or directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != nil {
				t.Setenv(registerTokenEnvVar, *tc.env)
			}
			_, err := initInputs(&registerArgs{
				InstanceAddr: "http://localhost:3000",
				TokenFile:    tc.tokenFile,
				RunnerName:   "runner",
				Ephemeral:    true,
				Labels:       " ubuntu:host , ubuntu:docker://node:18 ",
			})
			require.EqualError(t, err, tc.wantErr)
		})
	}

	for _, tc := range []struct {
		name, address, token, runnerName, wantToken string
		file                                        bool
	}{
		{name: "valid token", address: " http://localhost:3000 ", token: "from-plain-arg\n", runnerName: "runner\n", wantToken: "from-plain-arg"},
		{name: "valid token file", address: "http://localhost:3000", runnerName: "runner", file: true, wantToken: "from-file"},
		{name: "token from environment variable", address: "http://localhost:3000", runnerName: "runner", wantToken: "from-env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(registerTokenEnvVar, "from-env")
			var tokenFile string
			if tc.file {
				tokenFile = filepath.Join(t.TempDir(), "from-file")
				require.NoError(t, os.WriteFile(tokenFile, []byte("from-file\n"), 0o600))
			}
			inputs, err := initInputs(&registerArgs{
				InstanceAddr: tc.address,
				Token:        tc.token,
				TokenFile:    tokenFile,
				RunnerName:   tc.runnerName,
				Ephemeral:    true,
				Labels:       " ubuntu:host , ubuntu:docker://node:18 ",
			})
			require.NoError(t, err)
			require.Equal(t, "http://localhost:3000", inputs.InstanceAddr)
			require.Equal(t, tc.wantToken, inputs.Token)
			require.Equal(t, "runner", inputs.RunnerName)
			require.True(t, inputs.Ephemeral)
			require.Equal(t, []string{"ubuntu:host ", " ubuntu:docker://node:18"}, inputs.Labels)
		})
	}

	t.Run("empty labels", func(t *testing.T) {
		inputs, _ := initInputs(&registerArgs{
			Token:  "from-plain-arg",
			Labels: "  ",
		})
		require.Nil(t, inputs.Labels)
	})
}

func TestDoRegisterReturnsPingErrorWithoutRetrying(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.Runner.File = filepath.Join(t.TempDir(), ".runner")
	err := doRegister(t.Context(), cfg, &registerInputs{InstanceAddr: server.URL, RunnerName: "runner"})
	require.ErrorContains(t, err, "cannot ping the Gitea instance server")
	require.Equal(t, int32(1), requests.Load())
}
