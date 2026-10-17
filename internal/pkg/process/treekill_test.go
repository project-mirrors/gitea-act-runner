// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"context"
	"io"
	"os/exec"
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
		timeout time.Duration
		wantErr bool
	}{
		{name: "succeeds", timeout: time.Minute},
		{name: "fails once the context is done", timeout: 10 * time.Millisecond, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), tc.timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 &")
			cmd.Stdout = io.Discard
			cmd.SysProcAttr = SysProcAttr("", false)
			tk := NewTreeKill(cmd)
			cmd.WaitDelay = 50 * time.Millisecond
			require.NoError(t, cmd.Start())
			killer, err := tk.Capture(cmd.Process)
			require.NoError(t, err)
			t.Cleanup(func() { _ = killer.Kill() })

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
	cmd := exec.CommandContext(t.Context(), "sleep", "10")
	cmd.SysProcAttr = SysProcAttr("", false)
	tk := NewTreeKill(cmd)
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Wait() }()

	killer, err := tk.Capture(cmd.Process)
	require.NoError(t, err)
	require.NotNil(t, killer)
	require.NoError(t, cmd.Cancel())
	require.NoError(t, killer.Close())
}
