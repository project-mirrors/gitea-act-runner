// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"context"
	"io"

	"github.com/sirupsen/logrus"
)

// Libraries the runner depends on, such as the workflow model and the Docker client, log through
// logrus's global logger. Hand what they log to the process-wide Logger, so it follows the same
// level, format and secret masking as the rest of the output.
func init() {
	logrus.SetOutput(io.Discard)
	logrus.SetFormatter(discardFormatter{})
	logrus.SetLevel(toLogrusLevel(GetLevel()))
	logrus.AddHook(bridgeHook{})
}

type discardFormatter struct{}

func (discardFormatter) Format(*logrus.Entry) ([]byte, error) { return nil, nil }

type bridgeHook struct{}

func (bridgeHook) Levels() []logrus.Level { return logrus.AllLevels }

func (bridgeHook) Fire(e *logrus.Entry) error {
	l := Default()
	level := fromLogrusLevel(e.Level)
	ctx := e.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if !l.h.Enabled(ctx, level) {
		return nil
	}
	attrs := make([]Attr, 0, len(e.Data))
	for k, v := range e.Data {
		attrs = append(attrs, Attr{Key: k, Value: v})
	}
	r := Record{Time: e.Time, Level: level, Message: e.Message, Attrs: attrs}
	if e.Caller != nil {
		r.PC = e.Caller.PC + 1 // Frame.PC is one below the return address Record.PC holds
	}
	return l.h.Handle(ctx, r)
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

// LogrusBridgeIntact reports whether logrus's global logger is still set up as the bridge needs,
// discarding its own output and following the process-wide level. A dependency that calls
// logrus.SetOutput or SetLevel breaks that, and its lines then show up twice, or at the wrong level.
func LogrusBridgeIntact() bool {
	std := logrus.StandardLogger()
	if std.Out != io.Discard || std.GetLevel() != toLogrusLevel(GetLevel()) {
		return false
	}
	for _, level := range logrus.AllLevels {
		found := false
		for _, h := range std.Hooks[level] {
			if _, ok := h.(bridgeHook); ok {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
