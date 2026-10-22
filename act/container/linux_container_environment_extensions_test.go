// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerPath(t *testing.T) {
	type containerPathJob struct {
		destinationPath string
		sourcePath      string
		workDir         string
	}

	cwd, err := os.Getwd()
	require.NoError(t, err)
	tests := []containerPathJob{
		{"/home/act/go/src/github.com/nektos/act", "/home/act/go/src/github.com/nektos/act", ""},
		{"/home/act", `/home/act/`, ""},
		{cwd, ".", ""},
	}
	if runtime.GOOS == "windows" {
		rootDrive := os.Getenv("SystemDrive")
		tests = []containerPathJob{
			{"/mnt/c/Users/act/go/src/github.com/nektos/act", "C:\\Users\\act\\go\\src\\github.com\\nektos\\act\\", ""},
			{"/mnt/f/work/dir", `F:\work\dir`, ""},
			{"/mnt/c/windows/to/unix", "windows\\to\\unix", rootDrive + "\\"},
			{"/mnt/" + strings.ReplaceAll(strings.ToLower(rootDrive), `:`, "") + "/act", "act", rootDrive + "\\"},
		}
	}
	for _, test := range tests {
		if test.workDir != "" {
			t.Chdir(test.workDir)
		}
		assert.Equal(t, test.destinationPath, (&LinuxContainerEnvironmentExtensions{}).ToContainerPath(test.sourcePath))
	}
}

type typeAssertMockContainer struct {
	Container
	LinuxContainerEnvironmentExtensions
}

var _ ExecutionsEnvironment = &typeAssertMockContainer{}
