// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSaveAndLoadRegistration(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".runner")

	reg := &Registration{
		ID:        42,
		UUID:      "the-uuid",
		Name:      "runner",
		Token:     "the-token",
		Address:   "http://localhost:3000",
		Labels:    []string{"ubuntu:host", "ubuntu:docker://node:18"},
		Ephemeral: true,
	}

	require.NoError(t, SaveRegistration(file, reg))
	// SaveRegistration stamps the warning onto the in-memory struct
	require.Equal(t, registrationWarning, reg.Warning)

	loaded, err := LoadRegistration(file)
	require.NoError(t, err)

	// the warning is intentionally cleared on load
	require.Empty(t, loaded.Warning)
	loaded.Warning = reg.Warning
	require.Equal(t, reg, loaded)
}

func TestLoadRegistrationMissingFile(t *testing.T) {
	_, err := LoadRegistration(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
}

func TestLoadRegistrationInvalidJSON(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".runner")
	require.NoError(t, os.WriteFile(file, []byte("not json"), 0o600))

	_, err := LoadRegistration(file)
	require.Error(t, err)
}
