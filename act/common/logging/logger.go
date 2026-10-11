// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"context"
	"fmt"
	"runtime"
	"time"
)

const badKey = "!BADKEY"

// Logger writes printf-style messages with structured attributes to a Handler.
type Logger struct {
	h   Handler
	ctx context.Context
}

func New(h Handler) *Logger {
	return &Logger{h: h, ctx: context.Background()}
}

// With returns a Logger that adds the key/value pairs to every record, like slog.Logger.With.
func (l *Logger) With(args ...any) *Logger {
	if len(args) == 0 {
		return l
	}
	return &Logger{h: l.h.WithAttrs(argsToAttrs(args)), ctx: l.ctx}
}

func (l *Logger) WithError(err error) *Logger {
	return l.With("error", err)
}

// WithContext returns a Logger that passes ctx to its Handler.
func (l *Logger) WithContext(ctx context.Context) *Logger {
	return &Logger{h: l.h, ctx: ctx}
}

func (l *Logger) Enabled(level Level) bool {
	return l.h.Enabled(l.ctx, level)
}

func (l *Logger) Tracef(format string, args ...any) { l.logf(LevelTrace, format, args) }
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args) }

// Trace and the methods below format their arguments like fmt.Sprint.
func (l *Logger) Trace(args ...any) { l.log(LevelTrace, args) }
func (l *Logger) Debug(args ...any) { l.log(LevelDebug, args) }
func (l *Logger) Info(args ...any)  { l.log(LevelInfo, args) }
func (l *Logger) Warn(args ...any)  { l.log(LevelWarn, args) }
func (l *Logger) Error(args ...any) { l.log(LevelError, args) }

func (l *Logger) logf(level Level, format string, args []any) {
	if !l.Enabled(level) {
		return
	}
	l.emit(callerPC(4), level, fmt.Sprintf(format, args...))
}

func (l *Logger) log(level Level, args []any) {
	if !l.Enabled(level) {
		return
	}
	l.emit(callerPC(4), level, fmt.Sprint(args...))
}

// emit drops the Handler error, as a logger has nowhere to report it.
func (l *Logger) emit(pc uintptr, level Level, msg string) {
	_ = l.h.Handle(l.ctx, Record{Time: time.Now(), Level: level, Message: msg, PC: pc})
}

// callerPC returns the program counter skip frames up, 4 being the caller of an exported method.
func callerPC(skip int) uintptr {
	var pcs [1]uintptr
	runtime.Callers(skip, pcs[:])
	return pcs[0]
}

func argsToAttrs(args []any) []Attr {
	attrs := make([]Attr, 0, len(args)/2+1)
	for len(args) > 0 {
		switch k := args[0].(type) {
		case Attr:
			attrs = append(attrs, k)
			args = args[1:]
		case string:
			if len(args) == 1 {
				attrs = append(attrs, Attr{Key: badKey, Value: k})
				args = nil
			} else {
				attrs = append(attrs, Attr{Key: k, Value: args[1]})
				args = args[2:]
			}
		default:
			attrs = append(attrs, Attr{Key: badKey, Value: k})
			args = args[1:]
		}
	}
	return attrs
}
