// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !WITHOUT_DOCKER

package container

import (
	"errors"
	"os"
)

func copyDockerSocketPermissions(_, _ string, _ os.FileInfo) error {
	return errors.New("docker socket ownership cannot be preserved on Windows")
}
