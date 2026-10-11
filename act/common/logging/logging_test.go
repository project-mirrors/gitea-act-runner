// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging_test

import (
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"

	"github.com/sirupsen/logrus"
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

	prev := logging.Default()
	t.Cleanup(func() { logging.SetDefault(prev) })
	logging.SetDefault(l)
	logging.Infof("global")
	logging.Error("global")

	require.Len(t, h.AllRecords(), 4)
	for _, r := range h.AllRecords() {
		assert.True(t, strings.HasSuffix(frameOf(t, r.PC).Function, "TestCallerPC"), r.Message)
	}
}

func TestDefaultLevel(t *testing.T) {
	prev := logging.GetLevel()
	t.Cleanup(func() { logging.SetLevel(prev) })

	logging.SetLevel(logging.LevelWarn)
	assert.Equal(t, logging.LevelWarn, logging.GetLevel())
	assert.True(t, logging.Enabled(logging.LevelError))
	assert.False(t, logging.Enabled(logging.LevelInfo))
}

func TestSlogHandler(t *testing.T) {
	var buf strings.Builder
	slogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l := logging.New(logging.NewSlogHandler(slogger.Handler())).With("job", "j1")

	l.Tracef("dropped")
	assert.False(t, l.Enabled(logging.LevelTrace))
	l.WithError(errors.New("boom")).Errorf("fail %s", "x")

	out := buf.String()
	assert.Contains(t, out, "level=ERROR")
	assert.Contains(t, out, `msg="fail x"`)
	assert.Contains(t, out, "job=j1")
	assert.Contains(t, out, "error=boom")
}

func TestLogrusBridge(t *testing.T) {
	prevLevel, prevDefault := logging.GetLevel(), logging.Default()
	t.Cleanup(func() {
		logging.SetLevel(prevLevel)
		logging.SetReportCaller(false)
		logging.SetDefault(prevDefault)
	})
	logging.SetLevel(logging.LevelInfo)
	assert.False(t, logrus.IsLevelEnabled(logrus.DebugLevel))
	logging.SetLevel(logging.LevelTrace)
	logging.SetReportCaller(true)
	h := logtest.NewWithLevel(logging.LevelTrace)
	logging.SetDefault(logging.New(h))

	logrus.WithField("lib", "docker").Warnf("from a library %d", 1)
	_, _, line, _ := runtime.Caller(0)
	logrus.Debug("below")
	logrus.Trace("lowest")

	recs := h.AllRecords()
	require.Len(t, recs, 3)
	assert.Equal(t, "from a library 1", recs[0].Message)
	assert.Equal(t, logging.LevelWarn, recs[0].Level)
	v, _ := recs[0].Attr("lib")
	assert.Equal(t, "docker", v)
	assert.Equal(t, logging.LevelDebug, recs[1].Level)
	f := frameOf(t, recs[1].PC)
	assert.True(t, strings.HasSuffix(f.Function, "TestLogrusBridge"), f.Function)
	assert.Equal(t, line+1, f.Line)
	assert.Equal(t, logging.LevelTrace, recs[2].Level)

	h = logtest.New() // logrus passes Debug, the Logger does not
	logging.SetDefault(logging.New(h))
	logrus.Debug("dropped")
	assert.Empty(t, h.AllRecords())
}

func TestLogrusBridgeIntact(t *testing.T) {
	assert.True(t, logging.LogrusBridgeIntact())

	prev := logrus.StandardLogger().Out
	t.Cleanup(func() { logrus.SetOutput(prev) })
	logrus.SetOutput(&strings.Builder{}) // what a dependency doing this would do
	assert.False(t, logging.LogrusBridgeIntact())
	logrus.SetOutput(prev)
	assert.True(t, logging.LogrusBridgeIntact())

	prevLevel := logrus.GetLevel()
	t.Cleanup(func() { logrus.SetLevel(prevLevel) })
	logrus.SetLevel(logrus.PanicLevel)
	assert.False(t, logging.LogrusBridgeIntact())
}
