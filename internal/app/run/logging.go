// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"gitea.com/gitea/runner/act/common/logging"
)

// NullLogger is used to create a new JobLogger to discard logs. This
// will prevent these logs from being logged to the stdout, but
// forward them to the Reporter via its handler.
type NullLogger struct{}

// WithJobLogger returns a handler that discards all logs.
func (n NullLogger) WithJobLogger() logging.Handler {
	return logging.DiscardHandler
}
