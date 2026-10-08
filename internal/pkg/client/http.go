// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/pkg/ver"

	"connectrpc.com/connect"
	"gitea.dev/actionslib/ping/v1/pingv1connect"
	"gitea.dev/actionslib/pkg/protocol"
	"gitea.dev/actionslib/runner/v1/runnerv1connect"
)

func NewTransport(endpoint string, tlsConfig *tls.Config) *http.Transport {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10, // All requests go to one host; default is 2 which causes frequent reconnects.
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if strings.HasPrefix(endpoint, "https://") {
		transport.TLSClientConfig = tlsConfig
	}
	return transport
}

func getHTTPClient(endpoint string, tlsConfig *tls.Config, timeout time.Duration, extraHeaders map[string]string) *http.Client {
	return &http.Client{
		Transport: NewTransport(endpoint, tlsConfig),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 { // a custom CheckRedirect replaces net/http's own limit
				return errors.New("stopped after 10 redirects")
			}
			from, to := via[0].URL, req.URL
			if from.Hostname() != to.Hostname() || from.Scheme == "https" && to.Scheme != "https" {
				for name, value := range extraHeaders {
					if req.Header.Get(name) == value {
						req.Header.Del(name)
					}
				}
			}
			return nil
		},
	}
}

// New returns a new runner client. timeout bounds every RPC: without it a
// stalled connection parks the reporter for the whole job context, so logs and
// heartbeats stop together and the task is reaped as a zombie.
func New(endpoint string, tlsConfig *tls.Config, uuid, token string, timeout time.Duration, extraHeaders map[string]string, opts ...connect.ClientOption) *HTTPClient {
	baseURL := strings.TrimRight(endpoint, "/") + "/api/actions"

	opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			for name, value := range extraHeaders {
				if req.Header().Get(name) == "" { // keep protocol headers such as Content-Type
					req.Header().Set(name, value)
				}
			}
			req.Header().Set("User-Agent", "gitea-runner/"+ver.Version())
			if uuid != "" {
				req.Header().Set(protocol.UUIDHeader, uuid)
			}
			if token != "" {
				req.Header().Set(protocol.TokenHeader, token)
			}
			return next(ctx, req)
		}
	})))

	httpClient := getHTTPClient(endpoint, tlsConfig, timeout, extraHeaders)
	return &HTTPClient{
		PingServiceClient: pingv1connect.NewPingServiceClient(
			httpClient,
			baseURL,
			opts...,
		),
		RunnerServiceClient: runnerv1connect.NewRunnerServiceClient(
			httpClient,
			baseURL,
			opts...,
		),
		endpoint: endpoint,
	}
}

func (c *HTTPClient) Address() string {
	return c.endpoint
}

var _ Client = (*HTTPClient)(nil)

// An HTTPClient manages communication with the runner API.
type HTTPClient struct {
	pingv1connect.PingServiceClient
	runnerv1connect.RunnerServiceClient
	endpoint string
}
