// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"context"
	"log/slog"
	"slices"
)

func init() {
	slog.SetDefault(slog.New(slogBridge{}))
}

// slogBridge hands what libraries log through slog's global logger to the process-wide Logger.
type slogBridge struct {
	attrs []Attr
}

func (b slogBridge) Enabled(ctx context.Context, level slog.Level) bool {
	return Default().h.Enabled(ctx, Level(level))
}

func (b slogBridge) Handle(ctx context.Context, r slog.Record) error {
	h := Default().h
	if len(b.attrs) > 0 {
		h = h.WithAttrs(b.attrs)
	}
	attrs := make([]Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, fromSlogAttr(a))
		return true
	})
	return h.Handle(ctx, Record{Time: r.Time, Level: Level(r.Level), Message: r.Message, PC: r.PC, Attrs: attrs})
}

func (b slogBridge) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := slices.Clone(b.attrs)
	for _, a := range attrs {
		out = append(out, fromSlogAttr(a))
	}
	return slogBridge{attrs: out}
}

func (b slogBridge) WithGroup(string) slog.Handler { return b }

func fromSlogAttr(a slog.Attr) Attr {
	return Attr{Key: a.Key, Value: a.Value.Resolve().Any()}
}

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
