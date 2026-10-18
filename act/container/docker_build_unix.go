// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !WITHOUT_DOCKER && (linux || darwin || netbsd)

package container

import (
	"io/fs"
	"syscall"
)

func hardlinkInode(info fs.FileInfo) (uint64, bool) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && !info.IsDir() && stat.Nlink > 1 {
		return stat.Ino, true
	}
	return 0, false
}
