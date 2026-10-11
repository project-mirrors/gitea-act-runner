// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import "github.com/sirupsen/logrus"

var std = New(NewLogrusHandler(logrus.StandardLogger()))

// Default returns the process-wide Logger.
func Default() *Logger { return std }

// SetLevel sets the minimum level of the process-wide Logger.
func SetLevel(level Level) { logrus.SetLevel(toLogrusLevel(level)) }

// GetLevel returns the minimum level of the process-wide Logger.
func GetLevel() Level { return fromLogrusLevel(logrus.GetLevel()) }

func Enabled(level Level) bool { return std.Enabled(level) }

func With(args ...any) *Logger          { return std.With(args...) }
func WithError(err error) *Logger       { return std.WithError(err) }
func Tracef(format string, args ...any) { std.logf(LevelTrace, format, args) }
func Debugf(format string, args ...any) { std.logf(LevelDebug, format, args) }
func Infof(format string, args ...any)  { std.logf(LevelInfo, format, args) }
func Warnf(format string, args ...any)  { std.logf(LevelWarn, format, args) }
func Errorf(format string, args ...any) { std.logf(LevelError, format, args) }
func Trace(args ...any)                 { std.log(LevelTrace, args) }
func Debug(args ...any)                 { std.log(LevelDebug, args) }
func Info(args ...any)                  { std.log(LevelInfo, args) }
func Warn(args ...any)                  { std.log(LevelWarn, args) }
func Error(args ...any)                 { std.log(LevelError, args) }
