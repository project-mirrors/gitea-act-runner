// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"path/filepath"
	"strings"
)

func dirContainsPath(dir, target string) bool {
	dir = strings.ToLower(filepath.Clean(dir))
	target = strings.ToLower(filepath.Clean(target))
	if dir == "." || target == "." {
		return false
	}
	if dir == target {
		return true
	}
	return strings.HasPrefix(target, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}
