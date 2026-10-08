// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
)

func TestCompatibility(t *testing.T) {
	if skipReason != "" {
		t.Skip(skipReason)
	}

	sharedPoller := startRunner(t, "", "ubuntu-latest", runnerOptions{capacity: 16})
	t.Cleanup(func() {
		select {
		case <-sharedPoller.Done():
			t.Error("shared runner stopped")
		default:
		}
	})

	t.Run("payloads", testPayloads)
	t.Run("cache", testActionsCacheRoundTrip)
	t.Run("artifact", testArtifactRoundTrip)
	t.Run("cancellation_and_log_streaming", testRunCancellation)
	t.Run("dispatch", testWorkflowDispatch)
	t.Run("builtin_checkout", testBuiltinCheckout)
	t.Run("ephemeral", testEphemeralRunner)
	t.Run("kubernetes", testKubernetes)
}

func testPayloads(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const secretValue = "s3cr3t-value-xyz"

	api, repo := newScenario(t)
	if err := api.CreateSecret(ctx, repo, "FOO", secretValue); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := api.CreateVariable(ctx, repo, "GREETING", "hello-from-a-variable"); err != nil {
		t.Fatalf("create variable: %v", err)
	}
	if err := api.CreateVariable(ctx, repo, "E2E_SERVICE_IMAGE", os.Getenv("SERVICE_IMAGE")); err != nil {
		t.Fatalf("create service image variable: %v", err)
	}
	pushWorkflow(t, api, repo, "payloads.yml")

	wfRun := waitForRun(t, api, repo)
	requireSuccess(t, api, repo, wfRun.ID)

	logs := runLogs(t, api, repo, wfRun.ID)
	for _, want := range []string{
		"hello-from-a-variable",
		"plain-100%-done;-[bracket]",
		"multiline-first",
		"multiline-second",
		"notice-payload-here",
		"warning-payload-here",
		"error-payload-here",
		"group-payload-here",
		"inside-the-group",
		"received=produced-value-42",
		"cell-a-1",
		"cell-a-2",
		"cell-b-1",
		"cell-b-2",
		"imageless env=imageless-env-value host=imageless-host",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("stored logs are missing %q", want)
		}
	}
	if forwarded := commandRow(logs, "::notice::encoded-first"); forwarded != "::notice::encoded-first%0Aencoded-second" {
		t.Errorf("forwarded command row is %q, want its payload passed through unchanged", forwarded)
	}
	if !strings.Contains(logs, "plain-100%25-done") {
		t.Error("the emitted command did not escape %")
	}
	if strings.Contains(logs, secretValue) || !strings.Contains(logs, "***") {
		t.Error("job logs did not mask the secret")
	}

	if t.Failed() {
		t.Logf("full stored logs:\n%s", logs)
	}
}

func commandRow(logs, prefix string) string {
	for line := range strings.SplitSeq(logs, "\n") {
		_, payload, found := strings.Cut(line, "Z ")
		if found && strings.HasPrefix(strings.TrimRight(payload, "\r"), prefix) {
			return strings.TrimRight(payload, "\r")
		}
	}
	return ""
}
