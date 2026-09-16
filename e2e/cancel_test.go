// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func testRunCancellation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	api, repo := newScenario(t)
	err := api.CancelRun(ctx, repo, 0)
	cancelSupported := !errors.Is(err, ErrCancelUnsupported)
	var response statusError
	if cancelSupported && (!errors.As(err, &response) || response.code != http.StatusNotFound) {
		t.Fatalf("probe run cancellation: %v", err)
	}

	pushWorkflow(t, api, repo, "cancel.yml")

	wfRun := waitForRun(t, api, repo)
	waitForRunningJobLog(t, api, repo, wfRun.ID, "e2e-live-log-marker")
	if !cancelSupported {
		requireSuccess(t, api, repo, wfRun.ID)
		return
	}

	if err := api.CancelRun(ctx, repo, wfRun.ID); err != nil {
		dumpRunLogs(t, api, repo, wfRun.ID)
		t.Fatalf("cancel run: %v", err)
	}
	completed, err := api.WaitForRunConclusion(ctx, repo, wfRun.ID, time.Minute)
	if err != nil {
		dumpRunLogs(t, api, repo, wfRun.ID)
		t.Fatalf("cancelled run did not finish promptly: %v", err)
	}
	if completed.Conclusion != "cancelled" {
		dumpRunLogs(t, api, repo, wfRun.ID)
		t.Fatalf("cancelled run concluded %q, want cancelled", completed.Conclusion)
	}
}

func waitForRunningJobLog(t *testing.T, api *GiteaAPI, repo string, runID int64, substr string) {
	t.Helper()
	// The budget covers scheduling and the container start, which a loaded runner makes slow.
	ctx, cancel := context.WithTimeout(t.Context(), runTimeout)
	defer cancel()

	status := "none"
	for {
		jobs, err := api.Jobs(ctx, repo, runID)
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		if len(jobs) > 0 {
			status = jobs[0].Status
			if status == "in_progress" {
				logs, err := api.JobLogs(ctx, repo, jobs[0].ID)
				if err == nil && commandRow(logs, substr) == substr {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			dumpRunLogs(t, api, repo, runID)
			t.Fatalf("job of run %d is %q and never logged %q", runID, status, substr)
		case <-time.After(pollInterval):
		}
	}
}
