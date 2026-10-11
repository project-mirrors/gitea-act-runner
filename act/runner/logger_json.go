// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common/logging"
)

// jobLogJSONHandler writes one JSON object per record, in the shape of logrus's JSONFormatter.
type jobLogJSONHandler struct {
	w     io.Writer
	mu    *sync.Mutex // shared by every handler derived with WithAttrs, as they share w
	attrs []logging.Attr
}

func newJobLogJSONHandler(w io.Writer) *jobLogJSONHandler {
	return &jobLogJSONHandler{w: w, mu: &sync.Mutex{}}
}

func (h *jobLogJSONHandler) Enabled(context.Context, logging.Level) bool { return true }

func (h *jobLogJSONHandler) WithAttrs(attrs []logging.Attr) logging.Handler {
	c := *h
	c.attrs = slices.Concat(h.attrs, attrs)
	return &c
}

func (h *jobLogJSONHandler) Handle(_ context.Context, r logging.Record) error {
	data := make(map[string]any, len(h.attrs)+len(r.Attrs)+3)
	for _, a := range slices.Concat(h.attrs, r.Attrs) {
		switch v := a.Value.(type) {
		case error:
			data[a.Key] = v.Error() // an error marshals to {}
		default:
			data[a.Key] = v
		}
	}
	for _, key := range []string{"time", "msg", "level"} {
		if v, ok := data[key]; ok { // keep the fixed keys from being overwritten
			data["fields."+key] = v
		}
	}
	data["time"] = r.Time.Format(time.RFC3339)
	data["msg"] = r.Message
	data["level"] = jsonLevel(r.Level)

	line, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal fields to JSON, %w", err)
	}
	line = append(line, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err = h.w.Write(line)
	return err
}

func jsonLevel(l logging.Level) string {
	if l.String() == "warn" {
		return "warning" // what logrus calls it
	}
	return l.String()
}
