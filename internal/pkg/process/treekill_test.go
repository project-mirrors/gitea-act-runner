// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewTreeKillConfiguresCommand(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sleep", "1")
	tk := NewTreeKill(cmd)

	require.NotNil(t, tk)
	require.NotNil(t, cmd.Cancel)
	require.Equal(t, treeKillWaitDelay, cmd.WaitDelay)
	require.NoError(t, cmd.Cancel())
}

func TestTreeKillWaitWithLeftoverHoldingOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	for _, tc := range []struct {
		name    string
		wantErr bool
	}{
		{name: "succeeds"},
		{name: "fails once the context is done", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 &")
			cmd.Stdout = io.Discard
			cmd.SysProcAttr = SysProcAttr("", false)
			tk := NewTreeKill(cmd)
			tk.GracefulCancel = true
			cmd.WaitDelay = 50 * time.Millisecond
			require.NoError(t, cmd.Start())
			killer, err := tk.Capture(cmd.Process)
			require.NoError(t, err)
			t.Cleanup(func() { _ = killer.Kill() })
			if tc.wantErr {
				cancel()
			}

			err = tk.Wait(ctx, cmd)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTreeKillCaptureStoresKiller(t *testing.T) {
	cmd, ready := newTestProcess(t, "child", false)
	tk := NewTreeKill(cmd)
	require.NoError(t, cmd.Start())

	killer, err := tk.Capture(cmd.Process)
	require.NoError(t, err)
	require.NotNil(t, killer)
	ready()
	require.NoError(t, cmd.Cancel())
	require.NoError(t, killer.Close())
}

func newTestProcess(t *testing.T, mode string, copiedOutput bool) (*exec.Cmd, func() int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessHelper$")
	cmd.Env = append(os.Environ(), "GITEA_PROCESS_HELPER="+mode)
	cmd.SysProcAttr = SysProcAttr("", false)
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	var output io.ReadCloser
	if copiedOutput {
		var writer *io.PipeWriter
		output, writer = io.Pipe()
		cmd.Stdout = writer
	} else {
		output, err = cmd.StdoutPipe()
		require.NoError(t, err)
	}
	stop := context.AfterFunc(ctx, func() {
		_ = input.Close()
		_ = output.Close()
	})
	t.Cleanup(func() {
		stop()
		_ = input.Close()
		_ = output.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, func() int {
		t.Helper()
		_, err := input.Write([]byte{1})
		require.NoError(t, err)
		var childPID int
		_, err = fmt.Fscan(output, &childPID)
		require.NoError(t, err, "process should report its PID")
		require.Positive(t, childPID)
		return childPID
	}
}

func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("GITEA_PROCESS_HELPER")
	switch mode {
	case "":
		return
	case "orphan-child":
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		_, err := fmt.Fprintln(os.Stdout, os.Getpid())
		require.NoError(t, err)
		<-interrupt
	case "child":
		_, err := fmt.Fprintln(os.Stdout, os.Getpid())
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, os.Stdin)
		require.NoError(t, err)
	default:
		var start [1]byte
		_, err := io.ReadFull(os.Stdin, start[:])
		require.NoError(t, err)
		child := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
		child.Env = append(os.Environ(), "GITEA_PROCESS_HELPER=child")
		if mode == "orphan" {
			child.Env = append(os.Environ(), "GITEA_PROCESS_HELPER=orphan-child")
		}
		child.Stdin = os.Stdin
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		require.NoError(t, child.Start())
		if mode == "orphan" {
			os.Exit(0)
		}
		require.NoError(t, child.Wait())
	}
	os.Exit(0)
}
