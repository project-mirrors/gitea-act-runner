// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// testCLIRegisterDaemon drives the built binary the way a user does: register, then daemon.
func testCLIRegisterDaemon(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	api, repo := newScenario(t)
	token, err := fixture.RegistrationToken(ctx, repo)
	if err != nil {
		t.Fatalf("get registration token: %v", err)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "gitea-runner")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, "..").CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}

	cfgFile := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`runner:
  file: %s
  fetch_interval: 250ms
  fetch_interval_max: 250ms
  state_report_interval: 500ms
  log_report_interval: 500ms
cache:
  dir: %s
`, filepath.Join(dir, ".runner"), filepath.Join(dir, "cache"))
	if fixture.network != "" {
		cfg += "container:\n  network: " + fixture.network + "\n"
	}
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	label := "e2e-cli:docker://" + os.Getenv("E2E_JOB_IMAGE")
	out, err := exec.CommandContext(ctx, bin, "register", "--no-interactive", "-c", cfgFile,
		"--instance", fixture.baseURL, "--token", token, "--name", repo, "--labels", label).CombinedOutput()
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".runner")); err != nil {
		t.Fatalf("register did not write the runner file: %v\n%s", err, out)
	}

	daemonLog := &syncBuffer{}
	daemon := exec.Command(bin, "daemon", "-c", cfgFile)
	daemon.Stdout, daemon.Stderr = daemonLog, daemonLog
	if err := daemon.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()
	t.Cleanup(func() {
		_ = daemon.Process.Kill() // no-op once the daemon has exited
	})

	pushWorkflow(t, api, repo, "cli.yml")
	wfRun := waitForRun(t, api, repo)
	requireSuccessOrDumpDaemon(t, api, repo, wfRun.ID, daemonLog)
	if logs := runLogs(t, api, repo, wfRun.ID); commandRow(logs, "e2e-cli-job-ran") == "" {
		t.Errorf("job logs are missing the marker:\n%s", logs)
	}

	if err := daemon.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal daemon: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("daemon did not exit cleanly on SIGTERM: %v\n%s", err, daemonLog)
		}
	case <-time.After(30 * time.Second):
		t.Errorf("daemon did not exit within 30s of SIGTERM\n%s", daemonLog)
	}
}

func requireSuccessOrDumpDaemon(t *testing.T, api *GiteaAPI, repo string, runID int64, daemonLog *syncBuffer) {
	t.Helper()
	completed, err := api.WaitForRunConclusion(context.WithoutCancel(t.Context()), repo, runID, runTimeout)
	if err != nil || completed.Conclusion != "success" {
		t.Logf("daemon output:\n%s", daemonLog)
		requireSuccess(t, api, repo, runID)
	}
}
