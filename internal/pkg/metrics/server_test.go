// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHealthAndReadyEndpoints(t *testing.T) {
	tests := []struct {
		name, path   string
		ready        bool
		reason, body string
		status       int
	}{
		{"ready", "/readyz", true, "ok", "ok", http.StatusOK},
		{"not ready", "/readyz", false, "low disk space", "low disk space", http.StatusServiceUnavailable},
		{"live when not ready", "/healthz", false, "low disk space", "ok", http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHTTPHandler(func() (bool, string) { return test.ready, test.reason })
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			assert.Equal(t, test.status, response.Code)
			assert.Equal(t, test.body, response.Body.String())
		})
	}
}
