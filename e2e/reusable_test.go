// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import "testing"

// testReusableWorkflow runs a local workflow_call workflow and consumes its output.
func testReusableWorkflow(t *testing.T) {
	t.Parallel()
	api, repo := newScenario(t)
	pushWorkflow(t, api, repo, "reusable_callee.yml")
	pushWorkflow(t, api, repo, "reusable_caller.yml")

	wfRun := waitForRun(t, api, repo)
	requireSuccess(t, api, repo, wfRun.ID)
	if logs := runLogs(t, api, repo, wfRun.ID); commandRow(logs, "e2e-reusable-ran") == "" {
		t.Errorf("the called workflow did not run:\n%s", logs)
	}
}
