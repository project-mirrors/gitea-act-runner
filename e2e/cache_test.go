// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import "testing"

func testCacheAndArtifactRoundTrips(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, workflow, label string
		options               runnerOptions
	}{
		{"cache_v2", "cache.yml", "e2e-cache", runnerOptions{cacheV2: new(true)}},
		{"cache_v1", "cache.yml", "e2e-cache", runnerOptions{cacheV2: new(false)}},
		{"artifact_through_the_cache_server", "artifact.yml", "e2e-artifact", runnerOptions{cacheV2: new(true)}},
		{"artifact_direct_with_no_reachable_cache", "artifact.yml", "e2e-artifact", runnerOptions{cacheHost: "cache.invalid", cacheV2: new(false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, repo, _ := startIsolatedScenario(t, tc.workflow, tc.label, tc.options)

			wfRun := waitForRun(t, api, repo)
			requireSuccess(t, api, repo, wfRun.ID)
		})
	}
}
