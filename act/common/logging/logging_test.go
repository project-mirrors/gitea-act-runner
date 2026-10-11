// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging_test

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]logging.Level{
		"trace": logging.LevelTrace, "DEBUG": logging.LevelDebug, "info": logging.LevelInfo,
		"warn": logging.LevelWarn, "warning": logging.LevelWarn,
		"error": logging.LevelError, "fatal": logging.LevelError, "panic": logging.LevelError,
	} {
		got, err := logging.ParseLevel(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := logging.ParseLevel("loud")
	require.Error(t, err)
	assert.Equal(t, "warn", logging.LevelWarn.String())
}

type countingStringer struct{ n *int }

func (c countingStringer) String() string { *c.n++; return "x" }

func TestLogger(t *testing.T) {
	h := logtest.NewWithLevel(logging.LevelTrace)
	l := logging.New(h)

	l.With("job", "j1").WithError(errors.New("boom")).Warnf("step %d", 2)
	l.Info("a", 1, 2, "b")
	l.With("k").With(7).Debug("x")

	recs := h.AllRecords()
	require.Len(t, recs, 3)
	assert.Equal(t, "step 2", recs[0].Message)
	assert.Equal(t, logging.LevelWarn, recs[0].Level)
	assert.False(t, recs[0].Time.IsZero())
	job, _ := recs[0].Attr("job")
	assert.Equal(t, "j1", job)
	errv, _ := recs[0].Attr("error")
	require.EqualError(t, errv.(error), "boom")
	assert.Equal(t, "a1 2b", recs[1].Message) // fmt.Sprint spacing
	bad, _ := recs[2].Attr("!BADKEY")
	assert.Equal(t, 7, bad)

	h.Reset()
	assert.Nil(t, h.LastRecord())
}

func TestLoggerSkipsFormattingWhenDisabled(t *testing.T) {
	n := 0
	l := logging.New(logging.NewLevelHandler(logtest.NewWithLevel(logging.LevelTrace), logging.LevelWarn))
	l.Debugf("%v", countingStringer{&n})
	assert.Zero(t, n)
	assert.False(t, l.Enabled(logging.LevelInfo))
}

func TestMultiHandler(t *testing.T) {
	assert.False(t, logging.DiscardHandler.Enabled(t.Context(), logging.LevelError))
	all, warnOnly := logtest.New(), logtest.NewWithLevel(logging.LevelTrace)
	l := logging.New(logging.NewMultiHandler(all, logging.NewLevelHandler(warnOnly, logging.LevelWarn), logging.DiscardHandler)).With("a", 1)
	l.Info("i")
	l.Error("e")
	assert.Len(t, all.AllRecords(), 2)
	require.Len(t, warnOnly.AllRecords(), 1)
	v, _ := warnOnly.LastRecord().Attr("a")
	assert.Equal(t, 1, v)
}

func frameOf(t *testing.T, pc uintptr) runtime.Frame {
	t.Helper()
	require.NotZero(t, pc)
	f, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	return f
}

func TestCallerPC(t *testing.T) {
	l, h := logtest.NewLogger()
	l.Infof("method")
	l.Info("method")
	for _, r := range h.AllRecords() {
		assert.True(t, strings.HasSuffix(frameOf(t, r.PC).Function, "TestCallerPC"), r.Message)
	}

	std := logrus.StandardLogger()
	prevOut, prevCaller, prevHooks := logging.Output(), std.ReportCaller, std.ReplaceHooks(logrus.LevelHooks{})
	t.Cleanup(func() {
		logging.SetOutput(prevOut)
		std.SetReportCaller(prevCaller)
		std.ReplaceHooks(prevHooks)
	})
	logging.SetOutput(io.Discard)
	std.SetReportCaller(true)
	hook := logrustest.NewGlobal()
	logging.Infof("global")
	logging.Error("global")

	require.Len(t, hook.AllEntries(), 2)
	for _, e := range hook.AllEntries() {
		require.NotNil(t, e.Caller, e.Message)
		assert.True(t, strings.HasSuffix(e.Caller.Function, "TestCallerPC"), e.Caller.Function)
	}
}

func TestLogrusHandler(t *testing.T) {
	logger, hook := logrustest.NewNullLogger()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetReportCaller(true)
	l := logging.New(logging.NewLogrusHandler(logger)).With("job", "j1")

	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h := logging.NewLogrusHandler(logger).WithAttrs([]logging.Attr{{Key: "base", Value: true}})
	require.NoError(t, h.Handle(t.Context(), logging.Record{Time: when, Level: logging.LevelWarn, Message: "timed"}))

	l.Tracef("dropped")
	assert.False(t, l.Enabled(logging.LevelTrace))
	l.WithError(errors.New("boom")).Errorf("fail %s", "x")
	l.Debugf("dbg")

	entries := hook.AllEntries()
	require.Len(t, entries, 3)
	assert.Equal(t, when, entries[0].Time)
	assert.Equal(t, logrus.WarnLevel, entries[0].Level)
	assert.Equal(t, true, entries[0].Data["base"])

	assert.Equal(t, logrus.ErrorLevel, entries[1].Level)
	assert.Equal(t, "fail x", entries[1].Message)
	assert.Equal(t, "j1", entries[1].Data["job"])
	require.EqualError(t, entries[1].Data[logrus.ErrorKey].(error), "boom")
	assert.Equal(t, logrus.DebugLevel, entries[2].Level)

	require.NotNil(t, entries[1].Caller)
	assert.True(t, strings.HasSuffix(entries[1].Caller.Function, "TestLogrusHandler"), entries[1].Caller.Function)
	assert.Contains(t, entries[1].Caller.File, "logging_test.go")
}

func TestDefaultLevel(t *testing.T) {
	prev := logging.GetLevel()
	t.Cleanup(func() { logging.SetLevel(prev) })

	logging.SetLevel(logging.LevelWarn)
	assert.Equal(t, logging.LevelWarn, logging.GetLevel())
	assert.True(t, logging.Enabled(logging.LevelError))
	assert.False(t, logging.Enabled(logging.LevelInfo))
}

func TestDefaultReplaceAndTextFormat(t *testing.T) {
	hook := logtest.SetDefault(t)
	logging.Warnf("captured %d", 1)
	require.NotNil(t, hook.LastRecord())
	assert.Equal(t, "captured 1", hook.LastRecord().Message)

	prevOut, prevOpts, prevCaller := logging.Output(), logging.GetTextFormat(), logging.ReportCaller()
	t.Cleanup(func() {
		logging.SetOutput(prevOut)
		logging.SetTextFormat(prevOpts)
		logging.SetReportCaller(prevCaller)
	})
	var buf strings.Builder
	logging.SetOutput(&buf)
	logging.SetTextFormat(logging.TextOptions{
		DisableTimestamp: true,
		Filter:           func(line []byte) []byte { return []byte(strings.ReplaceAll(string(line), "secret", "***")) },
	})
	logging.SetReportCaller(false)
	assert.NotNil(t, logging.GetTextFormat().Filter)
	logging.SetDefault(logging.New(logging.NewLogrusHandler(logrus.StandardLogger())))
	logging.With("job", "secret").Error("x")
	assert.Contains(t, buf.String(), `job=***`)
	assert.NotContains(t, buf.String(), "secret")
}
