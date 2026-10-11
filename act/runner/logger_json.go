// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common/logging"
)

// newJobLogJSONHandler writes one JSON object per record, in the shape of logrus's JSONFormatter.
func newJobLogJSONHandler(w io.Writer) *jobLogHandler {
	return &jobLogHandler{w: w, mu: &sync.Mutex{}, format: formatJSON}
}

func formatJSON(r logging.Record) ([]byte, error) {
	data := make(map[string]any, len(r.Attrs)+3)
	for _, a := range r.Attrs {
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
		return nil, fmt.Errorf("failed to marshal fields to JSON, %w", err)
	}
	return append(line, '\n'), nil
}

func jsonLevel(l logging.Level) string {
	if l.String() == "warn" {
		return "warning" // what logrus calls it
	}
	return l.String()
}
