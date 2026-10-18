// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux

package runner

func selinuxEnabled() bool { return false }
