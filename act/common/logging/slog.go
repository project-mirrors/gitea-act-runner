// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"context"
	"log/slog"
)

type slogHandler struct {
	h slog.Handler
}

// NewSlogHandler writes to h. Levels map one to one, as Level uses slog's values.
func NewSlogHandler(h slog.Handler) Handler {
	return slogHandler{h: h}
}

func (s slogHandler) Enabled(ctx context.Context, level Level) bool {
	return s.h.Enabled(ctx, slog.Level(level))
}

func (s slogHandler) Handle(ctx context.Context, r Record) error {
	rec := slog.NewRecord(r.Time, slog.Level(r.Level), r.Message, r.PC)
	rec.AddAttrs(toSlogAttrs(r.Attrs)...)
	return s.h.Handle(ctx, rec)
}

func (s slogHandler) WithAttrs(attrs []Attr) Handler {
	return slogHandler{h: s.h.WithAttrs(toSlogAttrs(attrs))}
}

func toSlogAttrs(attrs []Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = slog.Any(a.Key, a.Value)
	}
	return out
}
