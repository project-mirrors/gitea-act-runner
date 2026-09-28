// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/internal/app/poll"
)

const runTimeout = 3 * time.Minute

var nonRepoChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func repoName(t *testing.T) string {
	return strings.ToLower(nonRepoChars.ReplaceAllString(t.Name(), "-"))
}

var fixture *GiteaFixture

var skipReason string

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := dockerClient(ctx)
	if err != nil {
		skipReason = fmt.Sprintf("docker unavailable: %v", err)
		return m.Run()
	}

	f, err := StartGitea(ctx, cli)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start gitea fixture: %v\n", err)
		return 1
	}
	fixture = f
	defer func() { _ = fixture.Close(context.Background()) }()

	fmt.Fprintf(os.Stderr, "gitea fixture: image=%s version=%s\n", fixture.image, fixture.version)

	return m.Run()
}

func newScenario(t *testing.T) (*GiteaAPI, string) {
	t.Helper()

	repo := repoName(t)
	api := &GiteaAPI{baseURL: fixture.baseURL, token: fixture.adminToken}
	if err := api.CreateRepo(t.Context(), repo); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return api, repo
}

func startIsolatedScenario(t *testing.T, workflow, label string, options runnerOptions) (*GiteaAPI, string, *poll.Poller) {
	t.Helper()

	api, repo := newScenario(t)
	poller := startRunner(t, repo, label, options)
	pushWorkflow(t, api, repo, workflow)
	return api, repo, poller
}

func pushWorkflow(t *testing.T, api *GiteaAPI, repo, workflow string) {
	t.Helper()

	content, err := os.ReadFile("testdata/workflows/" + workflow)
	if err != nil {
		t.Fatalf("read workflow fixture %s: %v", workflow, err)
	}
	if err := api.CreateFile(t.Context(), repo, ".gitea/workflows/"+workflow, string(content), "add "+workflow); err != nil {
		t.Fatalf("push workflow %s: %v", workflow, err)
	}
}

func requireSuccess(t *testing.T, api *GiteaAPI, repo string, runID int64) {
	t.Helper()

	completed, err := api.WaitForRunConclusion(t.Context(), repo, runID, runTimeout)
	if err != nil {
		dumpRunLogs(t, api, repo, runID)
		t.Fatalf("wait for run: %v", err)
	}
	if completed.Conclusion != "success" {
		dumpRunLogs(t, api, repo, runID)
		t.Fatalf("run concluded %q, want success", completed.Conclusion)
	}
}

func runLogs(t *testing.T, api *GiteaAPI, repo string, runID int64) string {
	t.Helper()
	ctx := t.Context()

	jobs, err := api.Jobs(ctx, repo, runID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var all strings.Builder
	for _, job := range jobs {
		logs, err := api.JobLogs(ctx, repo, job.ID)
		if err != nil {
			t.Fatalf("job logs for %s: %v", job.Name, err)
		}
		all.WriteString(logs)
	}
	return all.String()
}

func dumpRunLogs(t *testing.T, api *GiteaAPI, repo string, runID int64) {
	t.Helper()
	ctx := t.Context()

	jobs, err := api.Jobs(ctx, repo, runID)
	if err != nil {
		t.Logf("dump run %d: list jobs: %v", runID, err)
		return
	}
	for _, job := range jobs {
		logs, err := api.JobLogs(ctx, repo, job.ID)
		if err != nil {
			t.Logf("job %q (%s): logs unavailable: %v", job.Name, job.Conclusion, err)
			continue
		}
		t.Logf("job %q concluded %q:\n%s", job.Name, job.Conclusion, logs)
	}
}

func waitForRun(t *testing.T, api *GiteaAPI, repo string) *ActionRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	for {
		runs, err := api.Runs(ctx, repo)
		if err != nil {
			t.Fatalf("get latest run: %v", err)
		}
		if len(runs) > 0 {
			return &runs[0]
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no run appeared for %s within timeout", repo)
		case <-time.After(pollInterval):
		}
	}
}
