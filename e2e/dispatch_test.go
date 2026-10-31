// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

func testWorkflowDispatch(t *testing.T, m mode) {
	result := runWorkflow(t, m, workflowSpec{files: []string{"dispatch.yml"}, inputs: map[string]string{"subject": "dispatch-input-value"}})
	if result.conclusion != "success" {
		t.Errorf("run concluded %q, want success", result.conclusion)
	}
}

// Gitea indexes workflows asynchronously after the Contents API commit.
func waitForDispatch(ctx context.Context, api *GiteaAPI, repo, workflow, branch string, inputs map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var lastErr error
	for {
		lastErr = api.DispatchWorkflow(ctx, repo, workflow, branch, inputs)
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
