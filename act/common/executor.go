// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"context"
	"fmt"
	"runtime/debug"

	"gitea.com/gitea/runner/act/common/logging"
)

// Executor define contract for the steps of a workflow
type Executor func(ctx context.Context) error

// Conditional define contract for the conditional predicate
type Conditional func(ctx context.Context) bool

// NewInfoExecutor is an executor that logs messages
func NewInfoExecutor(format string, args ...any) Executor {
	return func(ctx context.Context) error {
		Logger(ctx).Infof(format, args...)
		return nil
	}
}

// NewDebugExecutor is an executor that logs messages
func NewDebugExecutor(format string, args ...any) Executor {
	return func(ctx context.Context) error {
		Logger(ctx).Debugf(format, args...)
		return nil
	}
}

// NewPipelineExecutor creates a new executor from a series of other executors
func NewPipelineExecutor(executors ...Executor) Executor {
	if len(executors) == 0 {
		return NewErrorExecutor(nil)
	}
	var rtn Executor
	for _, executor := range executors {
		if rtn == nil {
			rtn = executor
		} else {
			rtn = rtn.Then(executor)
		}
	}
	return rtn
}

// NewErrorExecutor creates a new executor that always errors out
func NewErrorExecutor(err error) Executor {
	return func(ctx context.Context) error {
		return err
	}
}

func RecoverPanic(err *error) { // defer directly in workers to keep stacks in host logs
	if recovered := recover(); recovered != nil {
		logging.Errorf("panic: %v\n%s", recovered, debug.Stack())
		*err = fmt.Errorf("panic: %v", recovered)
	}
}

// NewParallelExecutor creates a new executor from a parallel of other executors
func NewParallelExecutor(parallel int, executors ...Executor) Executor {
	if len(executors) == 0 {
		return func(ctx context.Context) error {
			return ctx.Err()
		}
	}

	return func(ctx context.Context) error {
		work := make(chan Executor, len(executors))
		errs := make(chan error, len(executors))

		if 1 > parallel {
			logging.Debugf("Parallel tasks (%d) below minimum, setting to 1", parallel)
			parallel = 1
		}

		logging.Infof("NewParallelExecutor: Creating %d workers for %d executors", parallel, len(executors))

		for workerID := range parallel {
			go func() {
				logging.Debugf("Worker %d started", workerID)
				taskCount := 0
				for executor := range work {
					taskCount++
					logging.Debugf("Worker %d executing task %d", workerID, taskCount)
					// Recover from panics in executors to avoid crashing the worker
					// goroutine which would leave the runner process hung.
					// https://gitea.com/gitea/runner/issues/371
					errs <- func() (err error) {
						defer RecoverPanic(&err)
						return executor(ctx)
					}()
				}
				logging.Debugf("Worker %d finished (%d tasks executed)", workerID, taskCount)
			}()
		}

		for i := range executors {
			work <- executors[i]
		}
		close(work)

		// Executor waits all executors to cleanup these resources.
		var firstErr error
		for range executors {
			err := <-errs
			if firstErr == nil {
				firstErr = err
			}
		}

		if err := ctx.Err(); err != nil {
			return err
		}
		return firstErr
	}
}

// Then runs another executor if this executor succeeds
func (e Executor) Then(then Executor) Executor {
	return func(ctx context.Context) error {
		if err := e(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return then(ctx)
	}
}

// If only runs this executor if conditional is true
func (e Executor) If(conditional Conditional) Executor {
	return func(ctx context.Context) error {
		if conditional(ctx) {
			return e(ctx)
		}
		return nil
	}
}

// IfNot only runs this executor if conditional is true
func (e Executor) IfNot(conditional Conditional) Executor {
	return e.If(func(ctx context.Context) bool { return !conditional(ctx) })
}

// IfBool only runs this executor if conditional is true
func (e Executor) IfBool(conditional bool) Executor {
	return e.If(func(ctx context.Context) bool {
		return conditional
	})
}

// Finally adds an executor to run after other executor
func (e Executor) Finally(finally Executor) Executor {
	return func(ctx context.Context) error {
		err := e(ctx)
		err2 := finally(ctx)
		if err2 != nil {
			if err != nil {
				return fmt.Errorf("error occurred running finally: %w (original error: %w)", err2, err)
			}
			return fmt.Errorf("error occurred running finally: %w (original error: %v)", err2, err)
		}
		return err
	}
}
