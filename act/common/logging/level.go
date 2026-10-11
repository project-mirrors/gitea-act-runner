// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"fmt"
	"strings"
)

// Level is a log severity. The values match log/slog, with Trace added below Debug.
type Level int

const (
	LevelTrace Level = -8
	LevelDebug Level = -4
	LevelInfo  Level = 0
	LevelWarn  Level = 4
	LevelError Level = 8
)

func (l Level) String() string {
	switch {
	case l <= LevelTrace:
		return "trace"
	case l <= LevelDebug:
		return "debug"
	case l <= LevelInfo:
		return "info"
	case l <= LevelWarn:
		return "warn"
	default:
		return "error"
	}
}

// ParseLevel accepts every name logrus does. "fatal" and "panic" map to LevelError because
// the Logger has no level above it.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(s) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error", "fatal", "panic":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("not a valid log level: %q", s)
}
