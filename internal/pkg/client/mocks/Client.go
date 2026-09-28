// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package mocks

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"gitea.dev/actionslib/runner/v1"
	"github.com/stretchr/testify/mock"
)

type Client struct {
	mock.Mock
	AddressValue string
}

func (m *Client) Address() string {
	return m.AddressValue
}

func call[Request, Response any](ctx context.Context, m *Client, method string, request *connect.Request[Request]) (*connect.Response[Response], error) {
	returns := m.MethodCalled(method, ctx, request)
	if callback, ok := returns.Get(0).(func(context.Context, *connect.Request[Request]) (*connect.Response[Response], error)); ok {
		return callback(ctx, request)
	}
	var response *connect.Response[Response]
	if value := returns.Get(0); value != nil {
		var ok bool
		response, ok = value.(*connect.Response[Response])
		if !ok {
			panic(fmt.Sprintf("unexpected response type %T for %s", value, method))
		}
	}
	return response, returns.Error(1)
}

func (m *Client) Declare(ctx context.Context, request *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error) {
	return call[runnerv1.DeclareRequest, runnerv1.DeclareResponse](ctx, m, "Declare", request)
}

func (m *Client) FetchTask(ctx context.Context, request *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	return call[runnerv1.FetchTaskRequest, runnerv1.FetchTaskResponse](ctx, m, "FetchTask", request)
}

func (m *Client) UpdateLog(ctx context.Context, request *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	return call[runnerv1.UpdateLogRequest, runnerv1.UpdateLogResponse](ctx, m, "UpdateLog", request)
}

func (m *Client) UpdateTask(ctx context.Context, request *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	return call[runnerv1.UpdateTaskRequest, runnerv1.UpdateTaskResponse](ctx, m, "UpdateTask", request)
}

func NewClient(t interface {
	mock.TestingT
	Cleanup(func())
},
) *Client {
	client := &Client{}
	client.Test(t)
	t.Cleanup(func() { client.AssertExpectations(t) })
	return client
}
