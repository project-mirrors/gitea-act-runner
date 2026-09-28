// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/pkg/ver"

	"connectrpc.com/connect"
	"gitea.dev/actionslib/ping/v1/pingv1connect"
	"gitea.dev/actionslib/pkg/protocol"
	"gitea.dev/actionslib/runner/v1/runnerv1connect"
)

func getHTTPClient(endpoint string, insecure bool, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10, // All requests go to one host; default is 2 which causes frequent reconnects.
		IdleConnTimeout:     90 * time.Second,
	}
	if strings.HasPrefix(endpoint, "https://") && insecure {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// New returns a new runner client. timeout bounds every RPC: without it a
// stalled connection parks the reporter for the whole job context, so logs and
// heartbeats stop together and the task is reaped as a zombie.
func New(endpoint string, insecure bool, uuid, token string, timeout time.Duration, opts ...connect.ClientOption) *HTTPClient {
	baseURL := strings.TrimRight(endpoint, "/") + "/api/actions"

	opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
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

	httpClient := getHTTPClient(endpoint, insecure, timeout)
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
