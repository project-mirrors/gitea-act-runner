// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var gracePeriod = regexp.MustCompile(`terminationGracePeriodSeconds: (\d+)`)

// Without it Kubernetes SIGKILLs the pod 30s after SIGTERM, mid-job.
func TestManifestsSetTerminationGracePeriod(t *testing.T) {
	files, err := filepath.Glob("*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, file := range files {
		content, err := os.ReadFile(file)
		require.NoError(t, err)
		if !strings.Contains(string(content), "containers:") {
			continue
		}
		match := gracePeriod.FindStringSubmatch(string(content))
		require.NotNil(t, match, file)
		seconds, err := strconv.Atoi(match[1])
		require.NoError(t, err)
		assert.GreaterOrEqual(t, seconds, 3600, file)
	}
}
