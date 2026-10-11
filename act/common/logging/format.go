// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"runtime"
	"sync"

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
	textMu      sync.Mutex
	textOptions TextOptions
)

// SetTextFormat sets the text format of the process-wide Logger.
func SetTextFormat(o TextOptions) {
	textMu.Lock()
	defer textMu.Unlock()
	textOptions = o
	logrus.SetFormatter(&filteringFormatter{
		inner: &logrus.TextFormatter{
			DisableColors:    o.DisableColors,
			DisableTimestamp: o.DisableTimestamp,
			FullTimestamp:    o.FullTimestamp,
			CallerPrettyfier: o.CallerPrettyfier,
		},
		filter: o.Filter,
	})
}

// GetTextFormat returns what SetTextFormat last set, the zero value if it never was.
func GetTextFormat() TextOptions {
	textMu.Lock()
	defer textMu.Unlock()
	return textOptions
}

// SetReportCaller makes the process-wide Logger record where each call came from, which costs time.
func SetReportCaller(on bool) { logrus.SetReportCaller(on) }

func ReportCaller() bool { return logrus.StandardLogger().ReportCaller }

type filteringFormatter struct {
	inner  logrus.Formatter
	filter func([]byte) []byte
}

func (f *filteringFormatter) Format(e *logrus.Entry) ([]byte, error) {
	line, err := f.inner.Format(e)
	if err != nil || f.filter == nil {
		return line, err
	}
	return f.filter(line), nil
}
