// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func testEphemeralRunner(t *testing.T) {
	t.Parallel()
	api, repo, poller := startIsolatedScenario(t, "ephemeral.yml", "e2e-ephemeral", runnerOptions{ephemeral: true})

	wfRun := waitForRun(t, api, repo)
	requireSuccess(t, api, repo, wfRun.ID)

	select {
	case <-poller.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ephemeral runner was not deleted")
	}
	if !poller.Unregistered() {
		t.Fatal("ephemeral runner stopped without being deleted")
	}
}
