// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var selinuxEnabled = sync.OnceValue(func() bool {
	if !isSELinuxfs("/sys/fs/selinux") && !hasOtherSELinuxfs() {
		return false
	}
	label, _ := os.ReadFile("/proc/self/attr/current")
	return strings.TrimSuffix(string(label), "\x00") != "kernel"
})

func hasOtherSELinuxfs() bool {
	mounts, _ := os.ReadFile("/proc/self/mountinfo")
	for line := range strings.Lines(string(mounts)) {
		if strings.Contains(line, " - selinuxfs ") {
			if fields := strings.Fields(line); len(fields) > 4 && isSELinuxfs(fields[4]) {
				return true
			}
		}
	}
	return false
}

func isSELinuxfs(mountPoint string) bool {
	var fs unix.Statfs_t
	return unix.Statfs(mountPoint, &fs) == nil && uint32(fs.Type) == uint32(unix.SELINUX_MAGIC) && fs.Flags&unix.ST_RDONLY == 0
}
