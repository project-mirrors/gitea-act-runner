// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type cliRunner struct {
	api     *GiteaAPI
	repo    string
	bin     string
	cfgFile string
	dir     string
}

// newCLIRunner registers the built binary against a fresh repo with a single docker label.
func newCLIRunner(t *testing.T, label, extraRunnerConfig string) *cliRunner {
	t.Helper()
	api, repo := newScenario(t)
	token, err := fixture.RegistrationToken(t.Context(), repo)
	if err != nil {
		t.Fatalf("get registration token: %v", err)
	}

	dir := t.TempDir()
	bin, err := runnerBinary()
	if err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`runner:
  file: %s
  fetch_interval: 250ms
  fetch_interval_max: 250ms
  state_report_interval: 500ms
  log_report_interval: 500ms
%scache:
  dir: %s
`, filepath.Join(dir, ".runner"), extraRunnerConfig, filepath.Join(dir, "cache"))
	if fixture.network != "" {
		cfg += "container:\n  network: " + fixture.network + "\n"
	}
	c := &cliRunner{api: api, repo: repo, bin: bin, cfgFile: filepath.Join(dir, "config.yaml"), dir: dir}
	if err := os.WriteFile(c.cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), c.bin, "register", "--no-interactive", "-c", c.cfgFile,
		"--instance", fixture.baseURL, "--token", token, "--name", repo,
		"--labels", label+":docker://"+os.Getenv("E2E_JOB_IMAGE")).CombinedOutput()
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".runner")); err != nil {
		t.Fatalf("register did not write the runner file: %v\n%s", err, out)
	}
	return c
}

type cliDaemon struct {
	cmd    *exec.Cmd
	log    *syncBuffer
	exited chan error
}

func (c *cliRunner) startDaemon(t *testing.T, args ...string) *cliDaemon {
	t.Helper()
	d := &cliDaemon{log: &syncBuffer{}, exited: make(chan error, 1)}
	d.cmd = exec.Command(c.bin, append([]string{"daemon", "-c", c.cfgFile}, args...)...)
	d.cmd.Stdout, d.cmd.Stderr = d.log, d.log
	if err := d.cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	go func() { d.exited <- d.cmd.Wait() }()
	t.Cleanup(func() {
		_ = d.cmd.Process.Kill() // no-op once the daemon has exited
	})
	return d
}

// wait returns the daemon's exit error, failing the test when it is still running after timeout.
func (d *cliDaemon) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-d.exited:
		return err
	case <-time.After(timeout):
		t.Fatalf("daemon still running after %s\n%s", timeout, d.log)
		return nil
	}
}

func (d *cliDaemon) terminate(t *testing.T) {
	t.Helper()
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal daemon: %v", err)
	}
}

// requireSuccess waits for the run and dumps the daemon output next to the job logs on failure.
func (d *cliDaemon) requireSuccess(t *testing.T, c *cliRunner, runID int64) {
	t.Helper()
	completed, err := c.api.WaitForRunConclusion(t.Context(), c.repo, runID, runTimeout)
	if err != nil || completed.Conclusion != "success" {
		t.Logf("daemon output:\n%s", d.log)
		requireSuccess(t, c.api, c.repo, runID)
	}
}

// testCLIRegisterDaemon drives the built binary the way a user does: register, then daemon.
func testCLIRegisterDaemon(t *testing.T) {
	t.Parallel()
	c := newCLIRunner(t, "e2e-cli", "")
	d := c.startDaemon(t)

	pushWorkflow(t, c.api, c.repo, "cli.yml")
	wfRun := waitForRun(t, c.api, c.repo)
	d.requireSuccess(t, c, wfRun.ID)
	if logs := runLogs(t, c.api, c.repo, wfRun.ID); commandRow(logs, "e2e-cli-job-ran") == "" {
		t.Errorf("job logs are missing the marker:\n%s", logs)
	}

	d.terminate(t)
	if err := d.wait(t, 30*time.Second); err != nil {
		t.Errorf("daemon did not exit cleanly on SIGTERM: %v\n%s", err, d.log)
	}
}

// testCLIOnce checks that --once takes a single job and then exits on its own.
func testCLIOnce(t *testing.T) {
	t.Parallel()
	c := newCLIRunner(t, "e2e-once", "")
	d := c.startDaemon(t, "--once")

	pushWorkflow(t, c.api, c.repo, "cli_once.yml")
	wfRun := waitForRun(t, c.api, c.repo)
	if err := d.wait(t, runTimeout); err != nil {
		t.Errorf("daemon --once exited with %v\n%s", err, d.log)
	}

	jobs, err := c.api.Jobs(t.Context(), c.repo, wfRun.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var succeeded, pending int
	for _, job := range jobs {
		switch {
		case job.Conclusion == "success":
			succeeded++
		case job.Status != "in_progress" && job.Status != "completed":
			pending++
		}
	}
	if succeeded != 1 || pending != 1 {
		t.Errorf("got %d succeeded and %d waiting jobs, want 1 and 1: %+v\n%s", succeeded, pending, jobs, d.log)
	}
}

// testCLILabelsOverride checks that daemon --labels replaces the labels given at registration.
func testCLILabelsOverride(t *testing.T) {
	t.Parallel()
	c := newCLIRunner(t, "e2e-registered", "")
	d := c.startDaemon(t, "--labels", "e2e-override:docker://"+os.Getenv("E2E_JOB_IMAGE"))

	pushWorkflow(t, c.api, c.repo, "cli_labels.yml")
	wfRun := waitForRun(t, c.api, c.repo)
	d.requireSuccess(t, c, wfRun.ID)

	runners, err := c.api.Runners(t.Context(), c.repo)
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	for _, runner := range runners {
		if runner.Name != c.repo {
			continue
		}
		var labels []string
		for _, label := range runner.Labels {
			labels = append(labels, label.Name)
		}
		if !slices.Equal(labels, []string{"e2e-override"}) {
			t.Errorf("runner has labels %v on the server, want only the --labels override", labels)
		}
		return
	}
	t.Errorf("runner %q not found among %+v", c.repo, runners)
}

// testCLIShutdownDrains checks that SIGTERM lets a running job finish within shutdown_timeout.
func testCLIShutdownDrains(t *testing.T) {
	t.Parallel()
	c := newCLIRunner(t, "e2e-shutdown", "  shutdown_timeout: 1m\n")
	d := c.startDaemon(t)

	pushWorkflow(t, c.api, c.repo, "cli_shutdown.yml")
	wfRun := waitForRun(t, c.api, c.repo)
	waitForRunningJobLog(t, c.api, c.repo, wfRun.ID, "e2e-shutdown-started")

	d.terminate(t)
	if err := d.wait(t, runTimeout); err != nil {
		t.Errorf("daemon did not exit cleanly: %v\n%s", err, d.log)
	}
	if strings.Contains(d.log.String(), "cancelled in progress jobs") {
		t.Errorf("the running job was cancelled instead of drained\n%s", d.log)
	}
	d.requireSuccess(t, c, wfRun.ID)
	if logs := runLogs(t, c.api, c.repo, wfRun.ID); commandRow(logs, "e2e-shutdown-finished") == "" {
		t.Errorf("job did not run to the end:\n%s", logs)
	}
}

// testCLIShutdownCancels checks that a job still running at shutdown_timeout is cancelled.
func testCLIShutdownCancels(t *testing.T) {
	t.Parallel()
	c := newCLIRunner(t, "e2e-shutdown", "  shutdown_timeout: 1s\n")
	d := c.startDaemon(t)

	pushWorkflow(t, c.api, c.repo, "cli_shutdown.yml")
	wfRun := waitForRun(t, c.api, c.repo)
	waitForRunningJobLog(t, c.api, c.repo, wfRun.ID, "e2e-shutdown-started")

	d.terminate(t)
	if err := d.wait(t, runTimeout); err != nil {
		t.Errorf("daemon did not exit cleanly after cancelling the job: %v\n%s", err, d.log)
	}
	if !strings.Contains(d.log.String(), "cancelled in progress jobs") {
		t.Errorf("daemon did not report cancelling the running job\n%s", d.log)
	}
	completed, err := c.api.WaitForRunConclusion(t.Context(), c.repo, wfRun.ID, time.Minute)
	if err != nil {
		t.Fatalf("cancelled run was never finalized: %v", err)
	}
	if completed.Conclusion == "success" {
		t.Errorf("run concluded success although the job was cancelled")
	}
}

func testExecArtifacts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	bin, err := runnerBinary()
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := os.ReadFile("testdata/workflows/exec_artifact.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflows := t.TempDir()
	for _, name := range []string{"a.yml", "b.yml"} {
		if err := os.WriteFile(filepath.Join(workflows, name), workflow, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	workdir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://example.com/e2e/exec.git"},
		{"-c", "user.name=e2e", "-c", "user.email=e2e@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	artifacts := t.TempDir()
	args := []string{"exec", "-C", workdir, "-W", workflows, "-i", os.Getenv("E2E_JOB_IMAGE"), "--artifact-server-path", artifacts}
	if fixture.network != "" {
		args = append(args, "--network", fixture.network)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "ACTIONS_") })
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exec: %v\n%s", err, out)
	}
	if blobs, _ := filepath.Glob(filepath.Join(artifacts, "v4", "*", "*", "blob")); len(blobs) != 2 {
		t.Fatalf("want one v4 artifact blob per workflow run, got %v", blobs)
	}
}
