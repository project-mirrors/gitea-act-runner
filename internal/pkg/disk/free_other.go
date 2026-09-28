// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package disk

import "fmt"

// FreeBytes reports the space available to an unprivileged user on the volume holding path.
func FreeBytes(path string) (uint64, error) {
	return 0, fmt.Errorf("free disk space checks are not supported for %s", path)
}
