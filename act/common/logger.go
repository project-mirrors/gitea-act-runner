// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"context"

	"gitea.com/gitea/runner/act/common/logging"
)

type loggerContextKey string

const loggerContextKeyVal = loggerContextKey("logging.Logger")

// Logger returns the appropriate logger for current context
func Logger(ctx context.Context) *logging.Logger {
	if logger, ok := ctx.Value(loggerContextKeyVal).(*logging.Logger); ok {
		return logger
	}
	return logging.Default()
}

// WithLogger adds a value to the context for the logger
func WithLogger(ctx context.Context, logger *logging.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKeyVal, logger)
}

type loggerHandlerKey string

const loggerHandlerKeyVal = loggerHandlerKey("logging.Handler")

// LoggerHandler returns the extra handler the job logger sends its records to, or nil.
// It affects the job logger, not the global logger.
func LoggerHandler(ctx context.Context) logging.Handler {
	if h, ok := ctx.Value(loggerHandlerKeyVal).(logging.Handler); ok {
		return h
	}
	return nil
}

// WithLoggerHandler adds the extra handler for the job logger to the context
func WithLoggerHandler(ctx context.Context, h logging.Handler) context.Context {
	return context.WithValue(ctx, loggerHandlerKeyVal, h)
}

type stepIDsContextKey struct{}

// StepIDs returns the IDs of the step and the composite steps enclosing the current one, outermost first.
func StepIDs(ctx context.Context) []string {
	ids, _ := ctx.Value(stepIDsContextKey{}).([]string)
	return ids
}

// WithStepIDs replaces the step IDs in the context
func WithStepIDs(ctx context.Context, ids []string) context.Context {
	return context.WithValue(ctx, stepIDsContextKey{}, ids)
}
