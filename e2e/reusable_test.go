// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import "testing"

// testReusableWorkflow runs a local workflow_call workflow and consumes its output.
func testReusableWorkflow(t *testing.T, m mode) {
	result := runWorkflow(t, m, workflowSpec{
		files:    []string{"reusable_callee.yml", "reusable_caller.yml"},
		execArgs: []string{"--no-skip-checkout"},
	})
	if result.conclusion != "success" || !result.hasLine("e2e-reusable-ran") {
		t.Errorf("run concluded %q, want success with the called workflow's marker", result.conclusion)
	}
}
