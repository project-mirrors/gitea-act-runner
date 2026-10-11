// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Attr is a structured key/value pair attached to a log record.
type Attr struct {
	Key   string
	Value any
}

// Record is one log event. Attrs holds the call-site attributes only, the ones added with
// Logger.With are accumulated by the Handler.
type Record struct {
	Time    time.Time
	Level   Level
	Message string
	PC      uintptr // program counter of the logging call, 0 if unknown
	Attrs   []Attr
}

// Attr returns the value of the last attribute named key.
func (r Record) Attr(key string) (any, bool) {
	for _, a := range slices.Backward(r.Attrs) {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}

// Handler consumes records, and is shaped like slog.Handler so that a slog backend can replace
// the current one without touching callers. Handle must not modify the Attrs slice it is given.
type Handler interface {
	Enabled(ctx context.Context, level Level) bool
	Handle(ctx context.Context, r Record) error
	// WithAttrs returns a Handler that adds attrs to every record it handles.
	WithAttrs(attrs []Attr) Handler
}

type multiHandler []Handler

// NewMultiHandler fans every record out to all handlers that have its level enabled.
func NewMultiHandler(handlers ...Handler) Handler {
	return multiHandler(handlers)
}

func (m multiHandler) Enabled(ctx context.Context, level Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r Record) error {
	var errs []error
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r))
		}
	}
	return errors.Join(errs...)
}

func (m multiHandler) WithAttrs(attrs []Attr) Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

type discardHandler struct{}

// DiscardHandler drops every record.
var DiscardHandler Handler = discardHandler{}

func (discardHandler) Enabled(context.Context, Level) bool  { return false }
func (discardHandler) Handle(context.Context, Record) error { return nil }
func (d discardHandler) WithAttrs([]Attr) Handler           { return d }

type levelHandler struct {
	h        Handler
	minLevel Level
}

// NewLevelHandler passes only records at or above minLevel on to h.
func NewLevelHandler(h Handler, minLevel Level) Handler {
	return levelHandler{h: h, minLevel: minLevel}
}

func (l levelHandler) Enabled(ctx context.Context, level Level) bool {
	return level >= l.minLevel && l.h.Enabled(ctx, level)
}

func (l levelHandler) Handle(ctx context.Context, r Record) error {
	if r.Level < l.minLevel {
		return nil
	}
	return l.h.Handle(ctx, r)
}

func (l levelHandler) WithAttrs(attrs []Attr) Handler {
	return levelHandler{h: l.h.WithAttrs(attrs), minLevel: l.minLevel}
}
