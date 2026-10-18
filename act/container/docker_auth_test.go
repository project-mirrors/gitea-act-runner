// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !windows && !(WITHOUT_DOCKER || !(linux || darwin || netbsd))

package container

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDockerAuthConfigFromFileHelpersAndEnv(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-credential-fake"), []byte("#!/bin/sh\nread -r server\n[ \"$1\" = get ] && [ \"$server\" = helper.example.com ] && exec echo '{\"Username\":\"helped\",\"Secret\":\"pw\"}'\necho 'credentials not found in native keychain'\nexit 1\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	auth := func(credentials string) string { return base64.StdEncoding.EncodeToString([]byte(credentials)) }
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths": {"https://legacy.example.com/v1/": {"auth": "`+auth("legacy:pw")+`"}}, "credHelpers": {"helper.example.com": "fake", "gone.example.com": "fake", "env-legacy.example.com": ""}}`), 0o600))
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_AUTH_CONFIG", `{"auths": {"env.example.com": {"auth": "`+auth("env:pw")+`"}, "https://env-legacy.example.com/v1/": {"auth": "`+auth("env:pw")+`"}}}`)

	for image, want := range map[string]registry.AuthConfig{
		"legacy.example.com/app": {Username: "legacy", Password: "pw", ServerAddress: "https://legacy.example.com/v1/"},
		"helper.example.com/app": {Username: "helped", Password: "pw", ServerAddress: "helper.example.com"},
		"env.example.com/app":    {Username: "env", Password: "pw", ServerAddress: "env.example.com"},
		"gone.example.com/app":   {},
	} {
		got, err := LoadDockerAuthConfig(t.Context(), image)
		require.NoError(t, err)
		assert.Equal(t, want, got, image)
	}
	assert.Equal(t, map[string]registry.AuthConfig{
		"https://legacy.example.com/v1/":     {Username: "legacy", Password: "pw", ServerAddress: "https://legacy.example.com/v1/"},
		"helper.example.com":                 {Username: "helped", Password: "pw", ServerAddress: "helper.example.com"},
		"gone.example.com":                   {},
		"env.example.com":                    {Username: "env", Password: "pw", ServerAddress: "env.example.com"},
		"https://env-legacy.example.com/v1/": {Username: "env", Password: "pw", ServerAddress: "https://env-legacy.example.com/v1/"},
		"env-legacy.example.com":             {Username: "env", Password: "pw", ServerAddress: "https://env-legacy.example.com/v1/"},
	}, LoadDockerAuthConfigs(t.Context()))
}
