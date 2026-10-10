// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePlatform(t *testing.T) {
	t.Run("empty input returns nil platform without error", func(t *testing.T) {
		got, err := parsePlatform("")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("os/arch", func(t *testing.T) {
		got, err := parsePlatform("linux/amd64")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "linux", got.OS)
		assert.Equal(t, "amd64", got.Architecture)
		assert.Empty(t, got.Variant)
	})

	t.Run("os/arch/variant", func(t *testing.T) {
		got, err := parsePlatform("linux/arm/v7")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "linux", got.OS)
		assert.Equal(t, "arm", got.Architecture)
		assert.Equal(t, "v7", got.Variant)
	})

	t.Run("input is lowercased", func(t *testing.T) {
		got, err := parsePlatform("Linux/AMD64/V8")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "linux", got.OS)
		assert.Equal(t, "amd64", got.Architecture)
		assert.Equal(t, "v8", got.Variant)
	})

	for _, bad := range []string{
		"amd64",
		"linux",
		"linux/",
		"/amd64",
		"/",
		"//",
		"linux/arm/",
		"linux/arm/v7/extra",
	} {
		t.Run("rejects "+bad, func(t *testing.T) {
			got, err := parsePlatform(bad)
			require.Error(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestPlatformMatches(t *testing.T) {
	for _, testcase := range []struct {
		requested, image string
		want             bool
	}{
		{"linux/amd64", "linux/amd64", true},
		{"linux/amd64/v1", "linux/amd64", true},
		{"linux/arm64", "linux/amd64", false},
		{"linux/arm64/v8", "linux/arm64", true},
		{"linux/arm/v7", "linux/arm/v7", true},
		{"linux/arm", "linux/arm/v7", true},
		{"linux/arm/v6", "linux/arm/v7", false},
		{"linux/arm", "linux/arm/v6", true},
		{"windows/arm/v7", "linux/arm/v7", false},
	} {
		requested, err := parsePlatform(testcase.requested)
		require.NoError(t, err)
		image, err := parsePlatform(testcase.image)
		require.NoError(t, err)
		assert.Equal(t, testcase.want, platformMatches(requested, image), "%s on %s", testcase.requested, testcase.image)
	}
}
