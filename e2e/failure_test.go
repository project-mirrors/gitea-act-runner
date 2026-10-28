// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import "testing"

func testFailurePaths(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	api, repo := newScenario(t)
	pushWorkflow(t, api, repo, "failure.yml")

	wfRun := waitForRun(t, api, repo)
	completed, err := api.WaitForRunConclusion(ctx, repo, wfRun.ID, runTimeout)
	if err != nil {
		dumpRunLogs(t, api, repo, wfRun.ID)
		t.Fatalf("wait for run: %v", err)
	}
	if completed.Conclusion != "failure" {
		t.Errorf("run concluded %q, want failure", completed.Conclusion)
	}

	jobs, err := api.Jobs(ctx, repo, wfRun.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	want := map[string]string{
		"fail":      "failure",
		"tolerated": "success",
		"dependent": "skipped",
		"timeout":   "failure",
	}
	for _, job := range jobs {
		if w, ok := want[job.Name]; ok && job.Conclusion != w {
			t.Errorf("job %q concluded %q, want %q", job.Name, job.Conclusion, w)
		}
		delete(want, job.Name)
	}
	for name := range want {
		t.Errorf("job %q not found", name)
	}

	logs := runLogs(t, api, repo, wfRun.ID)
	for _, marker := range []string{"e2e-before-fail", "e2e-failure-cleanup", "e2e-tolerated-next", "e2e-timeout-started"} {
		if commandRow(logs, marker) != marker {
			t.Errorf("logs are missing %q", marker)
		}
	}
	if commandRow(logs, "e2e-skipped-step") != "" {
		t.Error("a success() step ran after a failed step")
	}
	if t.Failed() {
		t.Logf("full stored logs:\n%s", logs)
	}
}
