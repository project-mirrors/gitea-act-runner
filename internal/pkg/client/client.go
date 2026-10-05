// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"gitea.dev/actionslib/runner/v1"
)

// A Client manages communication with the runner.
type Client interface {
	Address() string
	Declare(context.Context, *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error)
	FetchTask(context.Context, *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error)
	UpdateLog(context.Context, *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error)
	UpdateTask(context.Context, *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error)
}

// IsRegistrationRejected reports whether Gitea itself answered "unregistered runner", unlike a proxy's 401.
func IsRegistrationRejected(err error) bool {
	var connectErr *connect.Error
	return errors.As(err, &connectErr) && strings.HasSuffix(connectErr.Message(), "unregistered runner")
}
