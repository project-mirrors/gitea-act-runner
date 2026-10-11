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
	logrus.SetLevel(logrus.TraceLevel) // the level filter is the process-wide Logger's
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
	return l.h.Handle(ctx, Record{Time: e.Time, Level: level, Message: e.Message, Attrs: attrs})
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
