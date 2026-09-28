// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

func testWorkflowDispatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	api, repo := newScenario(t)
	pushWorkflow(t, api, repo, "dispatch.yml")

	branch, err := api.DefaultBranch(ctx, repo)
	if err != nil {
		t.Fatalf("get default branch: %v", err)
	}
	inputs := map[string]string{"subject": "dispatch-input-value"}
	if err := waitForDispatch(ctx, api, repo, branch, inputs); err != nil {
		t.Fatalf("dispatch workflow: %v", err)
	}

	dispatched := waitForRun(t, api, repo)
	if dispatched.Event != "workflow_dispatch" {
		t.Fatalf("run event is %q, want workflow_dispatch", dispatched.Event)
	}
	requireSuccess(t, api, repo, dispatched.ID)
}

// Gitea indexes workflows asynchronously after the Contents API commit.
func waitForDispatch(ctx context.Context, api *GiteaAPI, repo, branch string, inputs map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var lastErr error
	for {
		lastErr = api.DispatchWorkflow(ctx, repo, "dispatch.yml", branch, inputs)
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(pollInterval):
		}
	}
}
