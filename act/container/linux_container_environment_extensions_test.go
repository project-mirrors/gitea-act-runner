// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common/logging"

	"github.com/stretchr/testify/assert"
)

func TestContainerPath(t *testing.T) {
	type containerPathJob struct {
		destinationPath string
		sourcePath      string
		workDir         string
	}

	linuxcontainerext := &LinuxContainerEnvironmentExtensions{}

	if runtime.GOOS == "windows" {
		cwd, err := os.Getwd()
		if err != nil {
			logging.Error(err)
		}

		rootDrive := os.Getenv("SystemDrive")
		rootDriveLetter := strings.ReplaceAll(strings.ToLower(rootDrive), `:`, "")
		for _, v := range []containerPathJob{
			{"/mnt/c/Users/act/go/src/github.com/nektos/act", "C:\\Users\\act\\go\\src\\github.com\\nektos\\act\\", ""},
			{"/mnt/f/work/dir", `F:\work\dir`, ""},
			{"/mnt/c/windows/to/unix", "windows\\to\\unix", rootDrive + "\\"},
			{fmt.Sprintf("/mnt/%v/act", rootDriveLetter), "act", rootDrive + "\\"},
		} {
			if v.workDir != "" {
				t.Chdir(v.workDir)
			}

			assert.Equal(t, v.destinationPath, linuxcontainerext.ToContainerPath(v.sourcePath))
		}

		t.Chdir(cwd)
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			logging.Error(err)
		}
		for _, v := range []containerPathJob{
			{"/home/act/go/src/github.com/nektos/act", "/home/act/go/src/github.com/nektos/act", ""},
			{"/home/act", `/home/act/`, ""},
			{cwd, ".", ""},
		} {
			assert.Equal(t, v.destinationPath, linuxcontainerext.ToContainerPath(v.sourcePath))
		}
	}
}

type typeAssertMockContainer struct {
	Container
	LinuxContainerEnvironmentExtensions
}

// Type assert Container + LinuxContainerEnvironmentExtensions implements ExecutionsEnvironment
var _ ExecutionsEnvironment = &typeAssertMockContainer{}
