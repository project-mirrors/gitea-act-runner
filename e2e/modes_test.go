// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type mode struct {
	exec, kubernetes bool
}

var allModes = []mode{{}, {kubernetes: true}, {exec: true}, {exec: true, kubernetes: true}}

func (m mode) String() string {
	driver, backend := "daemon", "docker"
	if m.exec {
		driver = "exec"
	}
	if m.kubernetes {
		backend = "kubernetes"
	}
	return driver + "_" + backend
}

func inAllModes(scenario func(*testing.T, mode)) func(*testing.T) {
	return func(t *testing.T) {
		t.Parallel()
		for _, m := range allModes {
			t.Run(m.String(), func(t *testing.T) {
				t.Parallel()
				scenario(t, m)
			})
		}
	}
}

type workflowSpec struct {
	files    []string
	inputs   map[string]string // dispatches files[0] instead of relying on push
	execArgs []string
}

type workflowResult struct {
	conclusion string
	jobs       map[string]string
	lines      []string
}

func (r *workflowResult) job(id string) string {
	return cmp.Or(r.jobs[id], "skipped") // exec reports nothing for a job it skipped
}

func (r *workflowResult) hasLine(line string) bool {
	return slices.Contains(r.lines, line)
}

func runWorkflow(t *testing.T, m mode, spec workflowSpec) *workflowResult {
	t.Helper()
	run := daemonWorkflow
	if m.exec {
		run = execWorkflow
	}
	result := run(t, m, spec)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s concluded %q, jobs %v, logs:\n%s", m, result.conclusion, result.jobs, strings.Join(result.lines, "\n"))
		}
	})
	return result
}

func daemonWorkflow(t *testing.T, m mode, spec workflowSpec) *workflowResult {
	t.Helper()
	ctx := t.Context()

	api, repo := newScenario(t)
	if m.kubernetes {
		cluster := requireCluster(t)
		startRunner(t, repo, "e2e-kubernetes", runnerOptions{image: cluster.jobImage, kubernetes: cluster.kubernetes, capacity: 16})
	}
	files := map[string]string{}
	for _, file := range spec.files {
		content, err := os.ReadFile(filepath.Join("testdata", "workflows", file))
		if err != nil {
			t.Fatalf("read workflow fixture %s: %v", file, err)
		}
		if m.kubernetes {
			content = bytes.ReplaceAll(content, []byte("runs-on: ubuntu-latest"), []byte("runs-on: e2e-kubernetes"))
		}
		files[".gitea/workflows/"+file] = string(content)
	}
	if err := api.CreateFiles(ctx, repo, files, "add workflows"); err != nil {
		t.Fatalf("push workflows: %v", err)
	}
	if spec.inputs != nil {
		branch, err := api.DefaultBranch(ctx, repo)
		if err != nil {
			t.Fatalf("get default branch: %v", err)
		}
		if err := waitForDispatch(ctx, api, repo, spec.files[0], branch, spec.inputs); err != nil {
			t.Fatalf("dispatch workflow: %v", err)
		}
	}

	wfRun := waitForRun(t, api, repo)
	completed, err := api.WaitForRunConclusion(ctx, repo, wfRun.ID, runTimeout)
	if err != nil {
		dumpRunLogs(t, api, repo, wfRun.ID)
		t.Fatalf("wait for run: %v", err)
	}
	jobs, err := api.Jobs(ctx, repo, wfRun.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	result := &workflowResult{conclusion: completed.Conclusion, jobs: map[string]string{}}
	for _, job := range jobs {
		result.jobs[job.Name] = job.Conclusion
	}
	for line := range strings.SplitSeq(runLogs(t, api, repo, wfRun.ID), "\n") {
		if _, payload, found := strings.Cut(line, "Z "); found {
			result.lines = append(result.lines, strings.TrimRight(payload, "\r"))
		}
	}
	return result
}

func execWorkflow(t *testing.T, m mode, spec workflowSpec) *workflowResult {
	t.Helper()
	ctx := t.Context()

	bin, err := runnerBinary()
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	workflows := filepath.Join(workdir, ".gitea", "workflows")
	if err := os.MkdirAll(workflows, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range spec.files {
		content, err := os.ReadFile(filepath.Join("testdata", "workflows", file))
		if err != nil {
			t.Fatalf("read workflow fixture %s: %v", file, err)
		}
		if err := os.WriteFile(filepath.Join(workflows, file), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://example.com/e2e/exec.git"},
		{"-c", "user.name=e2e", "-c", "user.email=e2e@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	args := []string{"exec", "--json", "-C", workdir, "-W", workflows, "--artifact-server-path", t.TempDir(), "--artifact-server-port", "0"}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "ACTIONS_") })
	if m.kubernetes {
		cluster := requireCluster(t)
		args = append(args, "-i", "kubernetes://"+cluster.jobImage)
		env = append(env, "KUBECONFIG="+cluster.kubernetes.Kubeconfig)
	} else {
		args = append(args, "-i", os.Getenv("E2E_JOB_IMAGE"), "--network", fixture.network)
	}
	if spec.inputs != nil {
		args = append(args, "-E", "workflow_dispatch")
		for name, value := range spec.inputs {
			args = append(args, "--input", name+"="+value)
		}
	}
	cmd := exec.CommandContext(ctx, bin, append(args, spec.execArgs...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		t.Fatalf("exec: %v\n%s", runErr, stderr.String())
	}

	result := &workflowResult{conclusion: "success", jobs: map[string]string{}}
	if runErr != nil {
		result.conclusion = "failure"
	}
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var entry struct {
			JobID     string `json:"jobID"`
			JobResult string `json:"jobResult"`
			Msg       string `json:"msg"`
			RawOutput bool   `json:"raw_output"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry.JobResult != "" {
			result.jobs[entry.JobID] = entry.JobResult
		}
		if entry.RawOutput {
			for line := range strings.SplitSeq(strings.TrimRight(entry.Msg, "\n"), "\n") {
				result.lines = append(result.lines, strings.TrimRight(line, "\r"))
			}
		}
	}
	if result.conclusion != "success" && len(result.jobs) == 0 {
		t.Fatalf("exec ran no job: %v\n%s", runErr, stderr.String())
	}
	return result
}
