// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !plan9

package lock

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTryLock(t *testing.T) {
	runnerFile := filepath.Join(t.TempDir(), ".runner")

	for range 2 {
		release, err := TryLock(runnerFile)
		require.NoError(t, err)
		_, err = TryLock(runnerFile)
		require.ErrorIs(t, err, ErrLocked)
		require.NoError(t, release())
	}
}

func TestTryLockUncreatable(t *testing.T) {
	_, err := TryLock(filepath.Join(t.TempDir(), "missing-dir", ".runner"))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrLocked)
}
