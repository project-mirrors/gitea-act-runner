// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gitea.com/gitea/runner/internal/pkg/labels"
	"gitea.com/gitea/runner/internal/pkg/ver"

	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestSetupLines(t *testing.T) {
	original := osReleasePath
	path := filepath.Join(t.TempDir(), "os-release")
	require.NoError(t, os.WriteFile(path, []byte("PRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\n"), 0o600))
	osReleasePath = path
	defer func() { osReleasePath = original }()

	r := &Runner{
		name: "gitea-com-gitea-0003",
		labels: labels.Labels{
			{Name: "ubuntu-latest", Schema: labels.SchemeDocker, Arg: "//node:20"},
			{Name: "ubuntu-22.04", Schema: labels.SchemeDocker, Arg: "//node:20"},
		},
	}
	taskCtx, err := structpb.NewStruct(map[string]any{
		"job":        "lint",
		"repository": "gitea/runner",
		"event_name": "pull_request",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"gitea-com-gitea-0003(version:" + ver.Version() + ")",
		"::group::Runner Information",
		"Runner labels: ubuntu-latest, ubuntu-22.04",
		"Task: 268506",
		"Job: lint",
		"Repository: gitea/runner",
		"Triggered by event: pull_request",
		"::endgroup::",
		"::group::Operating System",
		"Ubuntu 24.04.4 LTS",
		fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		"::endgroup::",
	}, r.setupLines(&runnerv1.Task{Id: 268506, Context: taskCtx}))
}

func TestSetupLinesInputs(t *testing.T) {
	original := osReleasePath
	osReleasePath = filepath.Join(t.TempDir(), "absent")
	defer func() { osReleasePath = original }()

	r := &Runner{name: "gitea-com-gitea-0003"}
	taskCtx, err := structpb.NewStruct(map[string]any{
		"job":        "test",
		"repository": "gitea/runner",
		"event_name": "workflow_dispatch",
		"event": map[string]any{
			"inputs": map[string]any{
				"with_default": "default",
				"required":     "required input",
				"boolean":      true,
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"gitea-com-gitea-0003(version:" + ver.Version() + ")",
		"::group::Runner Information",
		"Task: 1",
		"Job: test",
		"Repository: gitea/runner",
		"Triggered by event: workflow_dispatch",
		"::endgroup::",
		"::group::Inputs",
		"boolean: true",
		"required: required input",
		"with_default: default",
		"::endgroup::",
		"::group::Operating System",
		fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		"::endgroup::",
	}, r.setupLines(&runnerv1.Task{Id: 1, Context: taskCtx}))
}

func TestPrettyOSName(t *testing.T) {
	for _, tt := range []struct {
		name      string
		osRelease string
		want      string
		missing   bool
	}{
		{"quoted value", "NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\n", "Ubuntu 24.04.4 LTS", false},
		{"unquoted value", "PRETTY_NAME=Alpine Linux v3.21\n", "Alpine Linux v3.21", false},
		{"no pretty name", "NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\n", "", false},
		{"similar key", "IMAGE_PRETTY_NAME=\"Ubuntu Core 24\"\n", "", false},
		{"missing file", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "os-release")
			if !tt.missing {
				require.NoError(t, os.WriteFile(path, []byte(tt.osRelease), 0o600))
			}

			original := osReleasePath
			osReleasePath = path
			defer func() { osReleasePath = original }()

			assert.Equal(t, tt.want, prettyOSName())
		})
	}
}
