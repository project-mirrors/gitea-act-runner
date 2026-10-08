// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package client

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pingv1 "gitea.dev/actionslib/ping/v1"
	"gitea.dev/actionslib/pkg/protocol"
	"github.com/stretchr/testify/require"
)

func TestGetHTTPClientUsesProxyFromEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.example.com:8080")

	client := getHTTPClient("http://gitea.example.com", nil, time.Minute, nil)
	require.Equal(t, time.Minute, client.Timeout)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)

	req, err := http.NewRequest(http.MethodGet, "http://gitea.example.com/api/actions/ping", nil)
	require.NoError(t, err)

	proxyURL, err := transport.Proxy(req)
	require.NoError(t, err)
	require.NotNil(t, proxyURL)
	require.Equal(t, "http://proxy.example.com:8080", proxyURL.String())
}

func TestGetHTTPClientAppliesTLSConfigOnlyToHTTPS(t *testing.T) {
	tlsConfig := &tls.Config{}
	require.Same(t, tlsConfig, getHTTPClient("https://gitea.example.com", tlsConfig, time.Minute, nil).Transport.(*http.Transport).TLSClientConfig)
	require.Nil(t, getHTTPClient("http://gitea.example.com", tlsConfig, time.Minute, nil).Transport.(*http.Transport).TLSClientConfig)
}

func TestNewSetsBaseURLAndHeaders(t *testing.T) {
	var gotPath string
	gotHeaders := make(http.Header)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	// trailing slash must be trimmed before "/api/actions" is appended
	c := New(server.URL+"/", nil, "the-uuid", "the-token", time.Minute, map[string]string{
		"X-Proxy-Token":      "proxy-token",
		protocol.UUIDHeader:  "other-uuid",
		protocol.TokenHeader: "other-token",
		"Content-Type":       "text/plain",
	})
	// Address returns the endpoint as supplied (untrimmed)
	require.Equal(t, server.URL+"/", c.Address())

	// the call is expected to fail (server returns 500), we only assert what was sent
	_, _ = c.Ping(t.Context(), connect.NewRequest(&pingv1.PingRequest{Data: "hi"}))

	require.True(t, strings.HasPrefix(gotPath, "/api/actions/"), "unexpected path %q", gotPath)
	require.Equal(t, "the-uuid", gotHeaders.Get(protocol.UUIDHeader))
	require.Equal(t, "the-token", gotHeaders.Get(protocol.TokenHeader))
	require.Equal(t, "proxy-token", gotHeaders.Get("X-Proxy-Token"))
	require.Equal(t, "application/proto", gotHeaders.Get("Content-Type"))
}

func TestNewOmitsEmptyHeaders(t *testing.T) {
	gotHeaders := make(http.Header)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := New(server.URL, nil, "", "", time.Minute, nil)
	_, _ = c.Ping(t.Context(), connect.NewRequest(&pingv1.PingRequest{Data: "hi"}))

	require.Empty(t, gotHeaders.Get(protocol.UUIDHeader))
	require.Empty(t, gotHeaders.Get(protocol.TokenHeader))
}

func TestNewStopsRedirectLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	c := New(server.URL, nil, "", "", time.Minute, nil)
	_, err := c.Ping(t.Context(), connect.NewRequest(&pingv1.PingRequest{Data: "hi"}))
	require.ErrorContains(t, err, "stopped after 10 redirects")
}

func TestNewRedirectKeepsExtraHeadersOnlyOnSameHost(t *testing.T) {
	var gotHeaders http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()
	_, port, err := net.SplitHostPort(target.Listener.Addr().String())
	require.NoError(t, err)

	for _, tc := range []struct {
		newOrigin func(http.Handler) *httptest.Server
		host      string
		kept      bool
	}{
		{httptest.NewServer, "127.0.0.1", true},
		{httptest.NewServer, "localhost", false},
		{httptest.NewTLSServer, "127.0.0.1", false},
	} {
		gotHeaders = nil
		origin := tc.newOrigin(http.RedirectHandler("http://"+net.JoinHostPort(tc.host, port), http.StatusPermanentRedirect))
		c := New(origin.URL, &tls.Config{InsecureSkipVerify: true}, "the-uuid", "the-token", time.Minute, map[string]string{"X-Proxy-Token": "proxy-token", "Content-Type": "text/plain"})
		_, _ = c.Ping(t.Context(), connect.NewRequest(&pingv1.PingRequest{Data: "hi"}))
		origin.Close()

		require.NotNil(t, gotHeaders, origin.URL)
		require.Equal(t, tc.kept, gotHeaders.Get("X-Proxy-Token") != "", "%s to %s", origin.URL, tc.host)
		require.Equal(t, "the-token", gotHeaders.Get(protocol.TokenHeader))
		require.Equal(t, "application/proto", gotHeaders.Get("Content-Type"))
	}
}
