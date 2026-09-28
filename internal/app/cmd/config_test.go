// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runConfigCmd(t *testing.T, configFile string, args ...string) (string, string, error) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd := loadConfigCmd(&configFile)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestConfigCmdGeneratePrintsTheExample(t *testing.T) {
	out, _, err := runConfigCmd(t, "", "generate")
	require.NoError(t, err)
	assert.Equal(t, string(config.Example), out)
}

func TestConfigCmdInitWritesTheMinimalConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	out, _, err := runConfigCmd(t, file, "init")
	require.NoError(t, err)
	assert.Contains(t, out, file)
	content, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, config.Minimal, string(content))

	_, _, err = runConfigCmd(t, file, "init")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")

	_, _, err = runConfigCmd(t, file, "init", "--force")
	require.NoError(t, err)

	t.Chdir(t.TempDir())
	_, _, err = runConfigCmd(t, "", "init")
	require.NoError(t, err)
	assert.FileExists(t, defaultConfigFileNames[0])
}

// The subcommands only wire arguments through, so one pass over all of them is enough.
func TestConfigCmdEditsTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(file, []byte("runner:\n  labels:\n    - self-hosted\n"), 0o600))

	_, _, err := runConfigCmd(t, file, "set", "container.options", "--cpus 2")
	require.NoError(t, err)
	_, _, err = runConfigCmd(t, file, "add", "runner.labels", "ubuntu:docker://node:22")
	require.NoError(t, err)
	_, _, err = runConfigCmd(t, file, "remove", "runner.labels", "self-hosted")
	require.NoError(t, err)

	out, _, err := runConfigCmd(t, file, "get", "runner.labels")
	require.NoError(t, err)
	assert.Equal(t, "ubuntu:docker://node:22\n", out)

	out, _, err = runConfigCmd(t, file, "get", "container.options")
	require.NoError(t, err)
	assert.Equal(t, "--cpus 2\n", out)
}

func TestConfigCmdResolvesTheConfigFile(t *testing.T) {
	t.Run("falls back to the working directory", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("runner:\n  capacity: 2\n"), 0o600))
		t.Chdir(dir)

		out, errOut, err := runConfigCmd(t, "", "get", "runner.capacity")
		require.NoError(t, err)
		assert.Equal(t, "2\n", out)
		assert.Contains(t, errOut, "using config file")
	})

	t.Run("reports that none was found", func(t *testing.T) {
		t.Chdir(t.TempDir())

		_, _, err := runConfigCmd(t, "", "set", "runner.capacity", "4")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--config")
	})
}
