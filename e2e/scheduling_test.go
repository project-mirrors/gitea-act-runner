// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testLabelRouting checks that each job reaches the runner owning its label and that a
// job with no matching runner stays queued.
func testLabelRouting(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	api, repo := newScenario(t)
	startRunner(t, repo, "e2e-route-a", runnerOptions{host: true, name: "-a"})
	startRunner(t, repo, "e2e-route-b", runnerOptions{host: true, name: "-b"})
	pushWorkflow(t, api, repo, "route.yml")

	wfRun := waitForRun(t, api, repo)
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	var jobs map[string]ActionJob
	for {
		list, err := api.Jobs(ctx, repo, wfRun.ID)
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		jobs = map[string]ActionJob{}
		for _, job := range list {
			jobs[job.Name] = job
		}
		if jobs["a"].Status == "completed" && jobs["b"].Status == "completed" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("jobs a and b did not complete: %+v", list)
		case <-time.After(pollInterval):
		}
	}

	for name, suffix := range map[string]string{"a": "-a", "b": "-b"} {
		if got := jobs[name].RunnerName; !strings.HasSuffix(got, suffix) {
			t.Errorf("job %s ran on runner %q, want the one named *%s", name, got, suffix)
		}
		if jobs[name].Conclusion != "success" {
			t.Errorf("job %s concluded %q", name, jobs[name].Conclusion)
		}
	}
	// a and b were picked up after unmatched was queued, so a runner that could take it would have by now
	if status := jobs["unmatched"].Status; status == "in_progress" || status == "completed" {
		t.Errorf("job with no matching runner is %q, want it queued", status)
	}
}

// testCapacity checks that runner.capacity bounds how many jobs run at once.
func testCapacity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		capacity   int
		concurrent bool
	}{
		{"serial", 1, false},
		{"parallel", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, repo := newScenario(t)
			startRunner(t, repo, "e2e-capacity", runnerOptions{capacity: tc.capacity})
			pushWorkflow(t, api, repo, "capacity.yml")

			wfRun := waitForRun(t, api, repo)
			requireSuccess(t, api, repo, wfRun.ID)

			jobs, err := api.Jobs(t.Context(), repo, wfRun.ID)
			if err != nil || len(jobs) != 2 {
				t.Fatalf("list jobs: %v (%d jobs)", err, len(jobs))
			}
			var start, end [2]int64
			for i, job := range jobs {
				logs, err := api.JobLogs(t.Context(), repo, job.ID)
				if err != nil {
					t.Fatalf("job logs: %v", err)
				}
				start[i] = spanValue(t, logs, "span-start=")
				end[i] = spanValue(t, logs, "span-end=")
			}
			overlap := max(start[0], start[1]) < min(end[0], end[1])
			if overlap != tc.concurrent {
				t.Errorf("capacity %d: jobs overlapped=%v, want %v (starts %v ends %v)", tc.capacity, overlap, tc.concurrent, start, end)
			}
		})
	}
}

func spanValue(t *testing.T, logs, prefix string) int64 {
	t.Helper()
	row := commandRow(logs, prefix)
	n, err := strconv.ParseInt(strings.TrimPrefix(row, prefix), 10, 64)
	if err != nil {
		t.Fatalf("no %q value in logs:\n%s", prefix, logs)
	}
	return n
}
