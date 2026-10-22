// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

func TestKillerKillsTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")

	script := fmt.Sprintf(
		`$c = Start-Process powershell -PassThru -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 600'; `+
			`Set-Content -LiteralPath %q -Value $c.Id; Start-Sleep -Seconds 600`, pidFile)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	killer, err := NewKiller(cmd.Process)
	require.NoError(t, err)
	defer killer.Close()

	childPID := awaitChildPID(t, pidFile)

	require.NoError(t, killer.Kill())

	require.Eventually(t, func() bool {
		return !processAlive(cmd.Process.Pid) && !processAlive(childPID)
	}, 20*time.Second, 200*time.Millisecond, "parent and child should both be terminated")
}
