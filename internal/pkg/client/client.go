// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect/v2"
	runnerv1 "gitea.dev/actionslib/runner/v1"
)

// A Client manages communication with the runner.
type Client interface {
	Address() string
	Declare(context.Context, *runnerv1.DeclareRequest) (*runnerv1.DeclareResponse, error)
	FetchTask(context.Context, *runnerv1.FetchTaskRequest) (*runnerv1.FetchTaskResponse, error)
	UpdateLog(context.Context, *runnerv1.UpdateLogRequest) (*runnerv1.UpdateLogResponse, error)
	UpdateTask(context.Context, *runnerv1.UpdateTaskRequest) (*runnerv1.UpdateTaskResponse, error)
}

// IsRegistrationRejected reports whether Gitea itself answered "unregistered runner", unlike a proxy's 401.
func IsRegistrationRejected(err error) bool {
	var connectErr *connect.Error
	return errors.As(err, &connectErr) && strings.HasSuffix(connectErr.Message(), "unregistered runner")
}
