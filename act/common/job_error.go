// Copyright 2021 The Gitea Authors. All rights reserved.
// Copyright 2021 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"context"
)

type jobErrorContextKey string

const jobErrorContextKeyVal = jobErrorContextKey("job.error")

// JobError returns the job error for current context if any
func JobError(ctx context.Context) error {
	container, _ := ctx.Value(jobErrorContextKeyVal).(map[string]error)
	return container["error"]
}

func SetJobError(ctx context.Context, err error) {
	if container, ok := ctx.Value(jobErrorContextKeyVal).(map[string]error); ok {
		container["error"] = err
	}
}

// WithJobErrorContainer adds a value to the context as a container for an error
func WithJobErrorContainer(ctx context.Context) context.Context {
	return context.WithValue(ctx, jobErrorContextKeyVal, map[string]error{})
}
