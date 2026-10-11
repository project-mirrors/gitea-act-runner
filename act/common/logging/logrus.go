// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"context"
	"runtime"

	"github.com/sirupsen/logrus"
)

type logrusHandler struct {
	entry *logrus.Entry
}

// NewLogrusHandler writes to l, so its level, hooks, formatter and output all apply.
func NewLogrusHandler(l *logrus.Logger) Handler {
	return &logrusHandler{entry: logrus.NewEntry(l)}
}

func (h *logrusHandler) Enabled(_ context.Context, level Level) bool {
	return h.entry.Logger.IsLevelEnabled(toLogrusLevel(level))
}

func (h *logrusHandler) Handle(ctx context.Context, r Record) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e := h.entry.WithContext(ctx) // always a copy, so the fields below never reach h.entry
	if !r.Time.IsZero() {
		e = e.WithTime(r.Time)
	}
	if len(r.Attrs) > 0 {
		e = e.WithFields(attrsToFields(r.Attrs))
	}
	if r.PC != 0 && e.Logger.ReportCaller {
		// logrus only skips its own frames, so without this it would report the logging package
		frame, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
		e.Caller = &frame
	}
	e.Log(toLogrusLevel(r.Level), r.Message)
	return nil
}

func (h *logrusHandler) WithAttrs(attrs []Attr) Handler {
	return &logrusHandler{entry: h.entry.WithFields(attrsToFields(attrs))}
}

func attrsToFields(attrs []Attr) logrus.Fields {
	fields := make(logrus.Fields, len(attrs))
	for _, a := range attrs {
		fields[a.Key] = a.Value
	}
	return fields
}

func toLogrusLevel(l Level) logrus.Level {
	switch {
	case l <= LevelTrace:
		return logrus.TraceLevel
	case l <= LevelDebug:
		return logrus.DebugLevel
	case l <= LevelInfo:
		return logrus.InfoLevel
	case l <= LevelWarn:
		return logrus.WarnLevel
	default:
		return logrus.ErrorLevel
	}
}

// fromLogrusLevel maps logrus's Fatal and Panic to LevelError.
func fromLogrusLevel(l logrus.Level) Level {
	switch l {
	case logrus.TraceLevel:
		return LevelTrace
	case logrus.DebugLevel:
		return LevelDebug
	case logrus.InfoLevel:
		return LevelInfo
	case logrus.WarnLevel:
		return LevelWarn
	default:
		return LevelError
	}
}
