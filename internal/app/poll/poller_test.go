// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package poll

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/internal/pkg/client/mocks"
	"gitea.com/gitea/runner/internal/pkg/config"

	"connectrpc.com/connect/v2"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestPoller_WorkerStateCounters(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("FetchTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.FetchTaskRequest) (*runnerv1.FetchTaskResponse, error) {
			return &runnerv1.FetchTaskResponse{}, nil
		},
	)

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	p := &Poller{client: client, cfg: cfg}

	ctx := t.Context()
	s1 := &workerState{}
	s2 := &workerState{}

	require.Nil(t, p.fetchTask(ctx, s1))
	require.Nil(t, p.fetchTask(ctx, s2))

	assert.Equal(t, int64(1), s1.consecutiveEmpty, "worker 1 should only count its own empty response")
	assert.Equal(t, int64(1), s2.consecutiveEmpty, "worker 2 should only count its own empty response")

	require.Nil(t, p.fetchTask(ctx, s1))
	assert.Equal(t, int64(2), s1.consecutiveEmpty)
	assert.Equal(t, int64(1), s2.consecutiveEmpty, "worker 2's counter must not be affected by worker 1's empty fetches")
}

func TestPoller_FetchErrorIncrementsErrorsOnly(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("FetchTask", mock.Anything, mock.Anything).Return(nil, errors.New("network unreachable"))

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	p := &Poller{client: client, cfg: cfg}

	s := &workerState{}
	require.Nil(t, p.fetchTask(t.Context(), s))
	assert.Equal(t, int64(1), s.consecutiveErrors)
	assert.Equal(t, int64(0), s.consecutiveEmpty)
}

func TestPoller_FetchTimeoutIsNoSignal(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("FetchTask", mock.Anything, mock.Anything).Return(nil, context.DeadlineExceeded)

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	p := &Poller{client: client, cfg: cfg}
	p.lastPollFailed.Store(true)

	hook := logtest.SetDefault(t)

	s := &workerState{consecutiveErrors: 2}
	require.Nil(t, p.fetchTask(t.Context(), s))
	assert.Equal(t, int64(2), s.consecutiveErrors)
	assert.Equal(t, int64(1), s.consecutiveEmpty)
	assert.True(t, p.lastPollFailed.Load(), "a timeout must not clear a known failure")

	p.fetchTask(t.Context(), s)
	assert.Len(t, hook.AllRecords(), 1)
}

func TestPoller_FetchUnauthenticatedStopsPolling(t *testing.T) {
	for name, tc := range map[string]struct {
		fetchErr error
		rejected bool
	}{
		"proxy": {connect.NewError(connect.CodeUnauthenticated, "401 Unauthorized"), false},
		"gitea": {connect.NewError(connect.CodeUnknown, "rpc error: code = Unauthenticated desc = unregistered runner").WithRemote(), true},
	} {
		t.Run(name, func(t *testing.T) {
			client := mocks.NewClient(t)
			client.On("FetchTask", mock.Anything, mock.Anything).Return(nil, tc.fetchErr)

			cfg, err := config.LoadDefault("")
			require.NoError(t, err)
			p := New(cfg, client, nil)
			s := &workerState{}
			require.Nil(t, p.fetchTask(t.Context(), s))

			assert.True(t, p.Unregistered(), "runner should be marked unregistered")
			assert.Equal(t, tc.rejected, p.RegistrationRejected())
			assert.Equal(t, int64(0), s.consecutiveErrors, "unauthenticated must not drive error backoff")
			select {
			case <-p.pollingCtx.Done():
			default:
				t.Fatal("expected polling context to be cancelled after an Unauthenticated response")
			}
		})
	}
}

func TestPoller_CalculateInterval(t *testing.T) {
	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	cfg.Runner.FetchInterval = 2 * time.Second
	cfg.Runner.FetchIntervalMax = 60 * time.Second
	p := &Poller{cfg: cfg}

	cases := []struct {
		name         string
		empty, errs  int64
		wantInterval time.Duration
	}{
		{"first poll, no backoff", 0, 0, 2 * time.Second},
		{"single empty, still base", 1, 0, 2 * time.Second},
		{"two empties, doubled", 2, 0, 4 * time.Second},
		{"five empties, capped path", 5, 0, 32 * time.Second},
		{"many empties, capped at max", 20, 0, 60 * time.Second},
		{"errors drive backoff too", 0, 3, 8 * time.Second},
		{"max(empty, errors) wins", 2, 4, 16 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &workerState{consecutiveEmpty: tc.empty, consecutiveErrors: tc.errs}
			assert.Equal(t, tc.wantInterval, p.calculateInterval(s))
		})
	}

	cfg.Runner.FetchIntervalMax = 2 * time.Minute
	assert.Equal(t, 2*time.Minute, p.calculateInterval(&workerState{consecutiveEmpty: 7}))
}

func atomicMax(target *atomic.Int64, val int64) {
	for {
		old := target.Load()
		if val <= old || target.CompareAndSwap(old, val) {
			break
		}
	}
}

type mockRunner struct {
	delay          time.Duration
	running        atomic.Int64
	maxConcurrent  atomic.Int64
	totalCompleted atomic.Int64
}

type idleAwareRunner struct {
	mockRunner
	idleCalls atomic.Int64
}

type availabilityRunner struct {
	mockRunner
	ready  atomic.Bool
	reason string
}

func (r *availabilityRunner) CanAcceptTask(_ context.Context) (bool, string) {
	if r.ready.Load() {
		return true, "ok"
	}
	return false, r.reason
}

func TestPollerReady(t *testing.T) {
	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	poller := New(cfg, nil, &availabilityRunner{})

	ready, reason := poller.Ready(time.Second)
	assert.True(t, ready)
	assert.Equal(t, "ok", reason)

	poller.reportAvailability(false, "low disk space")
	ready, reason = poller.Ready(time.Second)
	assert.False(t, ready)
	assert.Equal(t, "low disk space", reason)

	poller.reportAvailability(true, "ok")
	poller.lastPollFailed.Store(true)
	poller.lastHealthyPoll.Store(time.Now().UnixNano())
	ready, _ = poller.Ready(time.Second)
	assert.True(t, ready, "transient polling errors should remain ready during grace")

	poller.lastHealthyPoll.Store(time.Now().Add(-2 * time.Second).UnixNano())
	ready, reason = poller.Ready(time.Second)
	assert.False(t, ready)
	assert.Equal(t, "unable to poll Gitea", reason)

	poller.shutdownPolling()
	ready, reason = poller.Ready(time.Second)
	assert.False(t, ready)
	assert.Equal(t, "runner is shutting down", reason)

	poller.unregistered.Store(true)
	ready, reason = poller.Ready(time.Second)
	assert.False(t, ready)
	assert.Equal(t, "runner is no longer registered", reason)
}

func TestPollerPausesAndResumesForLocalAvailability(t *testing.T) {
	var fetches atomic.Int64
	cli := mocks.NewClient(t)
	cli.On("FetchTask", mock.Anything, mock.Anything).Maybe().Run(func(mock.Arguments) {
		fetches.Add(1)
	}).Return(&runnerv1.FetchTaskResponse{}, nil)

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	cfg.Runner.FetchInterval = 10 * time.Millisecond
	cfg.Runner.FetchIntervalMax = 10 * time.Millisecond
	runner := &availabilityRunner{reason: "low disk space"}
	poller := New(cfg, cli, runner)

	var wg sync.WaitGroup
	wg.Go(poller.Poll)
	time.Sleep(40 * time.Millisecond)
	assert.Zero(t, fetches.Load(), "an unavailable runner must not fetch a task")

	runner.ready.Store(true)
	require.Eventually(t, func() bool {
		return fetches.Load() > 0
	}, time.Second, 10*time.Millisecond, "polling should resume after local recovery")

	shutdownCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, poller.Shutdown(shutdownCtx))
	wg.Wait()
}

func (m *mockRunner) Run(ctx context.Context, _ *runnerv1.Task) error {
	atomicMax(&m.maxConcurrent, m.running.Add(1))
	select {
	case <-time.After(m.delay):
	case <-ctx.Done():
	}
	m.running.Add(-1)
	m.totalCompleted.Add(1)
	return nil
}

func TestPollerRunIdleMaintenance(t *testing.T) {
	runner := &idleAwareRunner{}
	p := &Poller{runner: runner, jobsCtx: t.Context()}

	p.runIdleMaintenance()

	assert.Equal(t, int64(1), runner.idleCalls.Load())
}

func (m *idleAwareRunner) OnIdle(_ context.Context) {
	m.idleCalls.Add(1)
}

func TestPollerCallsOnIdle(t *testing.T) {
	for _, pollOnce := range []bool{false, true} {
		name := "Poll"
		if pollOnce {
			name = "PollOnce"
		}
		t.Run(name, func(t *testing.T) {
			cli := mocks.NewClient(t)
			cli.On("FetchTask", mock.Anything, mock.Anything).Return(&runnerv1.FetchTaskResponse{}, nil)

			cfg, err := config.LoadDefault("")
			require.NoError(t, err)
			if !pollOnce {
				cfg.Runner.Capacity = 1
			}
			cfg.Runner.FetchInterval = 10 * time.Millisecond
			cfg.Runner.FetchIntervalMax = 10 * time.Millisecond

			runner := &idleAwareRunner{}
			poller := New(cfg, cli, runner)
			var wg sync.WaitGroup
			wg.Go(func() {
				if pollOnce {
					poller.PollOnce(func() {})
				} else {
					poller.Poll()
				}
			})

			require.Eventually(t, func() bool {
				return runner.idleCalls.Load() > 0
			}, time.Second, 10*time.Millisecond)

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			require.NoError(t, poller.Shutdown(ctx))
			wg.Wait()
		})
	}
}

func TestPollerPollOnceCallsOnTaskFetchedBeforeRunningIt(t *testing.T) {
	cli := mocks.NewClient(t)
	cli.On("FetchTask", mock.Anything, mock.Anything).Return(&runnerv1.FetchTaskResponse{Task: &runnerv1.Task{Id: 1}}, nil)
	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	runner := &mockRunner{}
	runsSeenByCallback := int64(-1)
	New(cfg, cli, runner).PollOnce(func() { runsSeenByCallback = runner.running.Load() + runner.totalCompleted.Load() })
	assert.Zero(t, runsSeenByCallback)
	assert.Equal(t, int64(1), runner.totalCompleted.Load())
}

func TestPoller_ConcurrencyLimitedByCapacity(t *testing.T) {
	const (
		capacity   = 3
		totalTasks = 6
		taskDelay  = 50 * time.Millisecond
	)

	var (
		tasksReturned  atomic.Int64
		fetchConcur    atomic.Int64
		maxFetchConcur atomic.Int64
	)

	cli := mocks.NewClient(t)
	cli.On("FetchTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.FetchTaskRequest) (*runnerv1.FetchTaskResponse, error) {
			atomicMax(&maxFetchConcur, fetchConcur.Add(1))
			defer fetchConcur.Add(-1)

			n := tasksReturned.Add(1)
			if n <= totalTasks {
				return &runnerv1.FetchTaskResponse{
					Task: &runnerv1.Task{Id: n},
				}, nil
			}
			return &runnerv1.FetchTaskResponse{}, nil
		},
	)

	runner := &mockRunner{delay: taskDelay}

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	cfg.Runner.Capacity = capacity
	cfg.Runner.FetchInterval = 10 * time.Millisecond
	cfg.Runner.FetchIntervalMax = 10 * time.Millisecond

	poller := New(cfg, cli, runner)

	var wg sync.WaitGroup
	wg.Go(poller.Poll)

	require.Eventually(t, func() bool {
		return runner.totalCompleted.Load() >= totalTasks
	}, 2*time.Second, 10*time.Millisecond, "all tasks should complete")

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = poller.Shutdown(ctx)
	require.NoError(t, err)
	wg.Wait()

	assert.LessOrEqual(t, runner.maxConcurrent.Load(), int64(capacity),
		"concurrent running tasks must not exceed capacity")
	assert.GreaterOrEqual(t, runner.maxConcurrent.Load(), int64(2),
		"with 6 tasks and capacity 3, at least 2 should overlap")
	assert.Equal(t, int64(1), maxFetchConcur.Load(),
		"FetchTask must never be called concurrently (single poller)")
	assert.Equal(t, int64(totalTasks), runner.totalCompleted.Load(),
		"all tasks should have been executed")
}

func TestPoller_ShutdownForcesJobsOnTimeout(t *testing.T) {
	var served atomic.Bool
	cli := mocks.NewClient(t)
	cli.On("FetchTask", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ *runnerv1.FetchTaskRequest) (*runnerv1.FetchTaskResponse, error) {
			if served.CompareAndSwap(false, true) {
				return &runnerv1.FetchTaskResponse{
					Task: &runnerv1.Task{Id: 1},
				}, nil
			}
			return &runnerv1.FetchTaskResponse{}, nil
		},
	)

	runner := &mockRunner{delay: 30 * time.Second}

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	cfg.Runner.Capacity = 1
	cfg.Runner.FetchInterval = 10 * time.Millisecond
	cfg.Runner.FetchIntervalMax = 10 * time.Millisecond

	poller := New(cfg, cli, runner)

	var wg sync.WaitGroup
	wg.Go(poller.Poll)

	require.Eventually(t, func() bool {
		return runner.running.Load() == 1
	}, time.Second, 10*time.Millisecond, "task should start running")

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = poller.Shutdown(ctx)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 5*time.Second,
		"Shutdown must not block on the parked task; shutdownJobs() must run on timeout")

	wg.Wait()
	assert.Equal(t, int64(1), runner.totalCompleted.Load(),
		"the parked task must be cancelled and unwound")
}
