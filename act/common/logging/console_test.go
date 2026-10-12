// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func consoleLine(t *testing.T, opts TextOptions, terminal, reportCaller bool, build func(*Logger)) string {
	t.Helper()
	var out bytes.Buffer
	c := newConsole(&out)
	c.opts, c.terminal, c.reportCaller = opts, terminal, reportCaller
	c.level.Set(slog.LevelDebug)
	build(New(NewSlogHandler(&consoleHandler{c: c})))
	return out.String()
}

func TestConsolePlain(t *testing.T) {
	line := consoleLine(t, TextOptions{DisableTimestamp: true}, false, false, func(l *Logger) {
		l.With("job", "build x", "n", 3).WithError(errors.New("boom")).Warnf("hello world")
		l.With("msg", "clash", "empty", "").Info("")
	})
	assert.Equal(t, "level=warning msg=\"hello world\" error=boom job=\"build x\" n=3\n"+
		"level=info empty= fields.msg=clash\n", line)
}

func TestConsolePlainTimeFilterAndCaller(t *testing.T) {
	opts := TextOptions{
		CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			return "[fn]", "[" + strings.TrimPrefix(f.Function, "x") + "]"
		},
		Filter: func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("secret"), []byte("***")) },
	}
	line := consoleLine(t, opts, false, true, func(l *Logger) { l.With("k", "secret").Debugf("a secret") })
	assert.Regexp(t, `^time="[^"]+" level=debug msg="a \*\*\*" func="\[fn\]" file="\[.*TestConsolePlainTimeFilterAndCaller[^"]*\]" k=\*\*\*\n$`, line)
}

func TestConsoleColored(t *testing.T) {
	line := consoleLine(t, TextOptions{DisableTimestamp: true}, true, false, func(l *Logger) {
		l.With("job", "j").Warn("careful")
	})
	assert.Equal(t, "\x1b[33mWARN\x1b[0m "+"careful"+strings.Repeat(" ", 44-len("careful"))+"  \x1b[33mjob\x1b[0m=j\n", line)

	full := consoleLine(t, TextOptions{FullTimestamp: true}, true, false, func(l *Logger) { l.Error("bad") })
	assert.Regexp(t, `^\x1b\[31mERRO\x1b\[0m\[\d{4}-\d\d-\d\dT[^\]]+\] bad`, full)

	elapsed := consoleLine(t, TextOptions{}, true, false, func(l *Logger) { l.Info("x") })
	assert.Regexp(t, `^\x1b\[36mINFO\x1b\[0m\[\d{4}\] x`, elapsed)

	plain := consoleLine(t, TextOptions{DisableColors: true, DisableTimestamp: true}, true, false, func(l *Logger) { l.Info("x") })
	assert.Equal(t, "level=info msg=x\n", plain)
}

func TestConsoleLevelAndOutput(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out)
	h := &consoleHandler{c: c}
	assert.False(t, h.Enabled(t.Context(), slog.LevelDebug)) // info unless raised
	c.level.Set(slog.LevelError)
	assert.False(t, h.Enabled(t.Context(), slog.LevelWarn))
	assert.True(t, h.Enabled(t.Context(), slog.LevelError))

	rec := slog.NewRecord(time.Unix(0, 0), slog.LevelError, "m", 0)
	rec.AddAttrs(slog.String("c", "3"))
	require.NoError(t, h.WithAttrs([]slog.Attr{slog.Int("a", 1)}).Handle(t.Context(), rec))
	assert.Contains(t, out.String(), "a=1 c=3")
}
