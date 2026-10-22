// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWorkflow(t *testing.T) {
	ctx := t.Context()
	require.NoError(t, NewPipelineExecutor()(ctx))
	require.EqualError(t, NewErrorExecutor(errors.New("test error"))(ctx), "test error")

	runcount := 0
	increment := func(_ctx context.Context) error {
		runcount++
		return nil
	}
	require.NoError(t, NewPipelineExecutor(increment, increment)(ctx))
	assert.Equal(t, 2, runcount)
}

func concurrencyProbe(wantActive int32) (exec Executor, count, maxActive *atomic.Int32) {
	var counted, active, peak atomic.Int32
	var once sync.Once
	reached := make(chan struct{})

	return func(_ctx context.Context) error {
		counted.Add(1)
		running := active.Add(1)
		for {
			seen := peak.Load()
			if running <= seen || peak.CompareAndSwap(seen, running) {
				break
			}
		}
		if running >= wantActive {
			once.Do(func() { close(reached) })
		}
		<-reached
		active.Add(-1)
		return nil
	}, &counted, &peak
}

func TestNewParallelExecutor(t *testing.T) {
	for name, parallel := range map[string]int{"parallel": 2, "below minimum": 0} {
		t.Run(name, func(t *testing.T) {
			exec, count, maxActive := concurrencyProbe(int32(max(1, parallel)))
			require.NoError(t, NewParallelExecutor(parallel, exec, exec, exec)(t.Context()))
			assert.Equal(t, int32(3), count.Load())
			assert.Equal(t, int32(max(1, parallel)), maxActive.Load())
		})
	}
}

func TestNewParallelExecutorEmpty(t *testing.T) {
	assert := assert.New(t)

	ctx := t.Context()
	require.NoError(t, NewParallelExecutor(2)(ctx))

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	err := NewParallelExecutor(2)(canceledCtx)
	assert.ErrorIs(err, context.Canceled)
}

func TestNewParallelExecutorRunsAllTasksAfterFailureOrCancellation(t *testing.T) {
	for name, tc := range map[string]struct {
		canceled        bool
		parallel, tasks int
	}{
		"failure":              {false, 2, 5},
		"canceled":             {true, 2, 5},
		"canceled single task": {true, 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			var calls atomic.Int32
			executors := make([]Executor, tc.tasks)
			for i := range executors {
				executors[i] = func(_ctx context.Context) error {
					calls.Add(1)
					if i == tc.tasks/2 {
						return errors.New("fake error")
					}
					return nil
				}
			}

			err := NewParallelExecutor(tc.parallel, executors...)(ctx)
			if tc.canceled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.EqualError(t, err, "fake error")
			}
			assert.Equal(t, int32(tc.tasks), calls.Load())
		})
	}
}

func TestExecutorConditionalsAndFinally(t *testing.T) {
	ctx := t.Context()
	var calls []string
	record := func(name string) Executor {
		return func(_ctx context.Context) error {
			calls = append(calls, name)
			return nil
		}
	}

	require.NoError(t, record("if-true").If(func(context.Context) bool { return true })(ctx))
	require.NoError(t, record("if-false").If(func(context.Context) bool { return false })(ctx))
	require.NoError(t, record("if-not").IfNot(func(context.Context) bool { return false })(ctx))
	require.NoError(t, record("if-bool").IfBool(true)(ctx))
	require.NoError(t, record("main").Finally(record("finally"))(ctx))

	assert.Equal(t, []string{"if-true", "if-not", "if-bool", "main", "finally"}, calls)
}

func TestExecutorFinallyReturnsFinallyErrorWithOriginal(t *testing.T) {
	mainErr := errors.New("main failed")
	finalErr := errors.New("cleanup failed")

	err := NewErrorExecutor(mainErr).Finally(NewErrorExecutor(finalErr))(t.Context())
	require.EqualError(t, err, "error occurred running finally: cleanup failed (original error: main failed)")
}
