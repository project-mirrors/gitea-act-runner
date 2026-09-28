// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package disk reports free space on the volume holding a path. Platforms without an
// implementation return an error, so callers treat the check as unavailable rather than
// as a full disk.
package disk
