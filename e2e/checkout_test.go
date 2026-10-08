// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import "testing"

func testBuiltinCheckout(t *testing.T) {
	t.Parallel()

	api, repo := newScenario(t)
	pushWorkflow(t, api, repo, "checkout.yml")
	branchRun := waitForRun(t, api, repo)
	if err := api.CreateTag(t.Context(), repo, "v1"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	tagRun := waitForRunAfter(t, api, repo, branchRun.ID)
	requireSuccess(t, api, repo, branchRun.ID)
	requireSuccess(t, api, repo, tagRun.ID)
}
