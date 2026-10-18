// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !WITHOUT_DOCKER

package container

import "io/fs"

func hardlinkInode(_info fs.FileInfo) (uint64, bool) { return 0, false }
