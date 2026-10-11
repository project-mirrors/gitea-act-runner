// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync/atomic"

	"github.com/sirupsen/logrus"
)

// TextOptions describes how the process-wide Logger writes a record as text.
type TextOptions struct {
	DisableColors    bool
	DisableTimestamp bool
	FullTimestamp    bool
	// CallerPrettyfier renames the function and file reported with a record when ReportCaller is on.
	CallerPrettyfier func(*runtime.Frame) (function, file string)
	// Filter rewrites each formatted line, for example to mask secrets.
	Filter func(line []byte) []byte
}

var (
	std      atomic.Pointer[Logger]
	stdState = newConsole(os.Stderr)
)

func init() {
	std.Store(New(NewSlogHandler(&consoleHandler{c: stdState})))
}

// Default returns the process-wide Logger.
func Default() *Logger { return std.Load() }

// SetDefault replaces the process-wide Logger, which tests use to capture what the package
// functions log. SetLevel, GetLevel, SetOutput and the format functions keep configuring the
// initial console Logger.
func SetDefault(l *Logger) { std.Store(l) }

// SetLevel sets the minimum level of the initial process-wide Logger and of what libraries log through logrus.
func SetLevel(level Level) {
	stdState.level.Set(slog.Level(level))
	logrus.SetLevel(toLogrusLevel(level))
}

// GetLevel returns the minimum level of the initial process-wide Logger.
func GetLevel() Level { return Level(stdState.level.Level()) }

// SetOutput sets where the initial process-wide Logger writes.
func SetOutput(w io.Writer) { stdState.setOutput(w) }

// Output returns where the initial process-wide Logger writes.
func Output() io.Writer {
	stdState.mu.Lock()
	defer stdState.mu.Unlock()
	return stdState.out
}

// SetTextFormat sets the text format of the initial process-wide Logger.
func SetTextFormat(o TextOptions) {
	stdState.mu.Lock()
	defer stdState.mu.Unlock()
	stdState.opts = o
}

// SetReportCaller makes the initial process-wide Logger record where each call came from, which costs time.
func SetReportCaller(on bool) {
	stdState.mu.Lock()
	defer stdState.mu.Unlock()
	stdState.reportCaller = on
	logrus.SetReportCaller(on)
}

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
