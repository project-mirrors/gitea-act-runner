// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package logtest provides a Handler that keeps records in memory for assertions.
package logtest

import (
	"context"
	"slices"
	"sync"

	"gitea.com/gitea/runner/act/common/logging"
)

type store struct {
	mu      sync.Mutex
	records []logging.Record
}

// Handler records everything it receives. Loggers derived with WithAttrs share one store.
type Handler struct {
	store *store
	attrs []logging.Attr
}

func New() *Handler {
	return &Handler{store: &store{}}
}

func (h *Handler) Enabled(context.Context, logging.Level) bool { return true }

// Handle stores the record with the attributes accumulated by WithAttrs placed first.
func (h *Handler) Handle(_ context.Context, r logging.Record) error {
	r.Attrs = append(slices.Clone(h.attrs), r.Attrs...)
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.records = append(h.store.records, r)
	return nil
}

func (h *Handler) WithAttrs(attrs []logging.Attr) logging.Handler {
	return &Handler{store: h.store, attrs: append(slices.Clone(h.attrs), attrs...)}
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
