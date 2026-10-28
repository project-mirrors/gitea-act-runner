// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"os"
	"testing"
)

func testHostMode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	api, repo := newScenario(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	if err := api.CreateVariable(ctx, repo, "E2E_HOSTNAME", hostname); err != nil {
		t.Fatalf("create variable: %v", err)
	}
	startRunner(t, repo, "e2e-host", runnerOptions{host: true})
	pushWorkflow(t, api, repo, "host.yml")

	wfRun := waitForRun(t, api, repo)
	requireSuccess(t, api, repo, wfRun.ID)
	if logs := runLogs(t, api, repo, wfRun.ID); commandRow(logs, "e2e-host-job-ran") == "" {
		t.Errorf("job logs are missing the marker:\n%s", logs)
	}
}
