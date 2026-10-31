// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func testFailurePaths(t *testing.T, m mode) {
	result := runWorkflow(t, m, workflowSpec{files: []string{"failure.yml"}})
	if result.conclusion != "failure" {
		t.Errorf("run concluded %q, want failure", result.conclusion)
	}
	for job, want := range map[string]string{"fail": "failure", "tolerated": "success", "dependent": "skipped"} {
		if got := result.job(job); got != want {
			t.Errorf("job %q concluded %q, want %q", job, got, want)
		}
	}
	for _, marker := range []string{"e2e-before-fail", "e2e-failure-cleanup", "e2e-tolerated-next"} {
		if !result.hasLine(marker) {
			t.Errorf("logs are missing %q", marker)
		}
	}
	if result.hasLine("e2e-skipped-step") {
		t.Error("a success() step ran after a failed step")
	}
}

func testJobTimeout(t *testing.T) {
	t.Parallel()
	api, repo, _ := startIsolatedScenario(t, "timeout.yml", "e2e-timeout", runnerOptions{timeout: 15 * time.Second})
	wfRun := waitForRun(t, api, repo)
	completed, err := api.WaitForRunConclusion(t.Context(), repo, wfRun.ID, runTimeout)
	if err != nil {
		dumpRunLogs(t, api, repo, wfRun.ID)
		t.Fatalf("wait for run: %v", err)
	}
	if logs := runLogs(t, api, repo, wfRun.ID); completed.Conclusion != "failure" || commandRow(logs, "e2e-timeout-started") == "" {
		t.Errorf("run concluded %q, want failure after the job started, logs:\n%s", completed.Conclusion, logs)
	}
}
