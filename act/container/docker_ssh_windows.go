// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !WITHOUT_DOCKER

package container

import "syscall"

func sshSysProcAttr() *syscall.SysProcAttr { return nil }
