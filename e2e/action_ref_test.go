// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"
)

func testMovingActionRef(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	api, repo, _ := startIsolatedScenario(t, "action_ref.yml", "e2e-action-ref", runnerOptions{})
	const actionRepo = "e2e-moving-action"
	if err := api.CreateRepo(ctx, actionRepo); err != nil {
		t.Fatalf("create action repo: %v", err)
	}
	action, err := os.ReadFile("testdata/actions/moving.yml")
	if err != nil {
		t.Fatalf("read action fixture: %v", err)
	}
	if err := api.CreateFile(ctx, actionRepo, "action.yml", string(action), "add action"); err != nil {
		t.Fatalf("push action: %v", err)
	}
	branch, err := api.DefaultBranch(ctx, repo)
	if err != nil {
		t.Fatalf("get default branch: %v", err)
	}

	var previousID int64
	for revision := range 3 {
		name := fmt.Sprintf("revision-%d", revision)
		if err := api.CreateFile(ctx, actionRepo, name, name, "add "+name); err != nil {
			t.Fatalf("move the action ref: %v", err)
		}
		if err := waitForDispatch(ctx, api, repo, "action_ref.yml", branch, map[string]string{"revision": name}); err != nil {
			t.Fatalf("dispatch workflow: %v", err)
		}
		wfRun := waitForRunAfter(t, api, repo, previousID)
		requireSuccess(t, api, repo, wfRun.ID)
		previousID = wfRun.ID
	}
}
