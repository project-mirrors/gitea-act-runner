// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"io"
	"sync/atomic"

	"github.com/sirupsen/logrus"
)

var std atomic.Pointer[Logger]

func init() {
	std.Store(New(NewLogrusHandler(logrus.StandardLogger())))
}

// Default returns the process-wide Logger.
func Default() *Logger { return std.Load() }

// SetDefault replaces the process-wide Logger, which tests use to capture what the package
// functions log. SetLevel, GetLevel, SetOutput and the format functions keep configuring the
// initial logrus-backed Logger.
func SetDefault(l *Logger) { std.Store(l) }

// SetLevel sets the minimum level of the process-wide Logger.
func SetLevel(level Level) { logrus.SetLevel(toLogrusLevel(level)) }

// GetLevel returns the minimum level of the process-wide Logger.
func GetLevel() Level { return fromLogrusLevel(logrus.GetLevel()) }

// SetOutput sets where the process-wide Logger writes.
func SetOutput(w io.Writer) { logrus.SetOutput(w) }

// Output returns where the process-wide Logger writes.
func Output() io.Writer { return logrus.StandardLogger().Out }

func Enabled(level Level) bool { return Default().Enabled(level) }

func With(args ...any) *Logger          { return Default().With(args...) }
func WithError(err error) *Logger       { return Default().WithError(err) }
func Tracef(format string, args ...any) { Default().logf(LevelTrace, format, args) }
func Debugf(format string, args ...any) { Default().logf(LevelDebug, format, args) }
func Infof(format string, args ...any)  { Default().logf(LevelInfo, format, args) }
func Warnf(format string, args ...any)  { Default().logf(LevelWarn, format, args) }
func Errorf(format string, args ...any) { Default().logf(LevelError, format, args) }
func Trace(args ...any)                 { Default().log(LevelTrace, args) }
func Debug(args ...any)                 { Default().log(LevelDebug, args) }
func Info(args ...any)                  { Default().log(LevelInfo, args) }
func Warn(args ...any)                  { Default().log(LevelWarn, args) }
func Error(args ...any)                 { Default().log(LevelError, args) }
