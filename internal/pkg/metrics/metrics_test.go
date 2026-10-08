// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package metrics

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func TestResultToStatusLabel(t *testing.T) {
	tests := []struct {
		name   string
		result runnerv1.Result
		want   string
	}{
		{"success", runnerv1.Result_RESULT_SUCCESS, LabelStatusSuccess},
		{"failure", runnerv1.Result_RESULT_FAILURE, LabelStatusFailure},
		{"cancelled", runnerv1.Result_RESULT_CANCELLED, LabelStatusCancelled},
		{"skipped", runnerv1.Result_RESULT_SKIPPED, LabelStatusSkipped},
		{"unspecified", runnerv1.Result_RESULT_UNSPECIFIED, LabelStatusUnknown},
		{"out of range", runnerv1.Result(999), LabelStatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ResultToStatusLabel(tt.result))
		})
	}
	expired, cancel := context.WithDeadline(t.Context(), time.Time{})
	defer cancel()
	require.Equal(t, LabelStatusTimeout, JobStatus(expired, runnerv1.Result_RESULT_FAILURE))
	require.Equal(t, LabelStatusSuccess, JobStatus(expired, runnerv1.Result_RESULT_SUCCESS))
	require.Equal(t, LabelStatusCancelled, JobStatus(t.Context(), runnerv1.Result_RESULT_CANCELLED))
}

func TestInitAndDynamicMetricRegistration(t *testing.T) {
	oldRegistry := Registry
	t.Cleanup(func() {
		Registry = oldRegistry
	})

	Registry = prometheus.NewRegistry()
	initOnce = sync.Once{}

	Init()
	Init()
	RunnerInfo.WithLabelValues("test", "runner").Set(1)
	RegisterUptimeFunc(time.Now().Add(-time.Second))
	RegisterRunningJobsFunc(func() int64 { return 2 }, 4)
	RegisterStateFunc(func() string { return LabelStateIdle })
	RegisterStateFunc(func() string { return LabelStateBusy })

	metrics, err := Registry.Gather()
	require.NoError(t, err)

	require.True(t, hasMetric(metrics, "gitea_runner_info"))
	require.True(t, hasMetric(metrics, "gitea_runner_uptime_seconds"))
	require.True(t, hasMetric(metrics, "gitea_runner_job_running"))
	require.True(t, hasMetric(metrics, "gitea_runner_job_capacity_utilization_ratio"))
	states := map[string]float64{}
	for _, mf := range metrics {
		if mf.GetName() == "gitea_runner_state" {
			for _, m := range mf.GetMetric() {
				states[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
			}
		}
	}
	require.Equal(t, map[string]float64{LabelStateBusy: 1, LabelStateIdle: 0, LabelStateUnavailable: 0}, states)
}

func TestRegisterRunningJobsFuncZeroCapacity(t *testing.T) {
	oldRegistry := Registry
	t.Cleanup(func() { Registry = oldRegistry })
	Registry = prometheus.NewRegistry()

	RegisterRunningJobsFunc(func() int64 { return 3 }, 0)

	metrics, err := Registry.Gather()
	require.NoError(t, err)
	for _, mf := range metrics {
		if mf.GetName() == "gitea_runner_job_capacity_utilization_ratio" {
			require.Len(t, mf.GetMetric(), 1)
			require.InDelta(t, 0, mf.GetMetric()[0].GetGauge().GetValue(), 0)
			return
		}
	}
	t.Fatal("capacity utilization metric not gathered")
}

func TestStartServerCanBeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	StartServer(ctx, "127.0.0.1:0", nil)
	cancel()
}

func hasMetric(metrics []*dto.MetricFamily, name string) bool {
	for _, mf := range metrics {
		if strings.EqualFold(mf.GetName(), name) {
			return true
		}
	}
	return false
}
