// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package logtest provides a Handler that keeps records in memory for assertions.
package logtest

import (
	"context"
	"slices"
	"sync"
	"testing"

	"gitea.com/gitea/runner/act/common/logging"
)

type store struct {
	mu      sync.Mutex
	records []logging.Record
}

// Handler records what it receives at or above its level. Loggers derived with WithAttrs share one store.
type Handler struct {
	store    *store
	attrs    []logging.Attr
	minLevel logging.Level
}

// New records from LevelInfo up, like the logger of logrus's hooks/test.
func New() *Handler {
	return NewWithLevel(logging.LevelInfo)
}

// NewLogger returns a Logger writing to a new Handler from New, and that Handler.
func NewLogger() (*logging.Logger, *Handler) {
	h := New()
	return logging.New(h), h
}

func NewWithLevel(minLevel logging.Level) *Handler {
	return &Handler{store: &store{}, minLevel: minLevel}
}

func (h *Handler) Enabled(_ context.Context, level logging.Level) bool { return level >= h.minLevel }

// Handle stores the record with the attributes accumulated by WithAttrs placed first.
func (h *Handler) Handle(_ context.Context, r logging.Record) error {
	r.Attrs = append(slices.Clone(h.attrs), r.Attrs...)
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.records = append(h.store.records, r)
	return nil
}

func (h *Handler) WithAttrs(attrs []logging.Attr) logging.Handler {
	return &Handler{store: h.store, attrs: append(slices.Clone(h.attrs), attrs...), minLevel: h.minLevel}
}

func (h *Handler) AllRecords() []logging.Record {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return slices.Clone(h.store.records)
}

// LastRecord returns nil when nothing was logged.
func (h *Handler) LastRecord() *logging.Record {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if len(h.store.records) == 0 {
		return nil
	}
	r := h.store.records[len(h.store.records)-1]
	return &r
}

func (h *Handler) Reset() {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.records = nil
}

// SetDefault makes the process-wide Logger record into a new Handler until the test ends.
func SetDefault(t testing.TB) *Handler {
	t.Helper()
	prev := logging.Default()
	logger, h := NewLogger()
	logging.SetDefault(logger)
	t.Cleanup(func() { logging.SetDefault(prev) })
	return h
}
