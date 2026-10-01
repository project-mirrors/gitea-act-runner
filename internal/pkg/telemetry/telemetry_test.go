// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func setEnv(t *testing.T, env map[string]string) {
	t.Cleanup(func() { SetTracerProvider(tracenoop.NewTracerProvider()) })
	for _, variable := range os.Environ() {
		if key, _, _ := strings.Cut(variable, "="); strings.HasPrefix(key, "OTEL_") {
			t.Setenv(key, "")
		}
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
}

func TestSetup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	for _, tc := range []struct {
		env             map[string]string
		traces, metrics bool
		err             string
	}{
		{},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"}, traces: true},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": server.URL, "OTEL_TRACES_EXPORTER": "NONE"}, metrics: true},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": server.URL, "OTEL_TRACES_EXPORTER": "zipkin, OTLP", "OTEL_METRICS_EXPORTER": "console"}, traces: true, metrics: true},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_TRACES_EXPORTER": "NONE", "OTEL_METRICS_EXPORTER": "none"}},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_SDK_DISABLED": "TRUE"}},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, err: `OTLP protocol "grpc" is not supported, only http/protobuf is`},
	} {
		t.Run(fmt.Sprint(tc.env), func(t *testing.T) {
			setEnv(t, tc.env)
			gathered := false
			shutdown, err := Setup(t.Context(), "uuid", "runner", prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
				gathered = true
				return nil, nil
			}))
			if tc.err != "" {
				require.EqualError(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			_, disabled := tracer.(tracenoop.Tracer)
			assert.Equal(t, tc.traces, !disabled)
			require.NoError(t, shutdown(t.Context()))
			assert.Equal(t, tc.metrics, gathered)
		})
	}
}

func TestExportRetriesAndMatchesOTLP(t *testing.T) {
	traceBodies, metricBodies := make(chan []byte, 2), make(chan []byte, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Len(t, r.TLS.PeerCertificates, 1)
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		assert.Equal(t, "gzip", r.Header.Get("Content-Encoding"))
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/otlp/v1/metrics" {
			select {
			case metricBodies <- body:
			default:
			}
			return
		}
		assert.Equal(t, "/otlp/v1/traces", r.URL.Path)
		traceBodies <- body
		if len(traceBodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	server.StartTLS()
	defer server.Close()
	certificate := server.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	require.NoError(t, err)
	certificatePath, keyPath := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	require.NoError(t, os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600))
	setEnv(t, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": server.URL + "/otlp/", "OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20token",
		"OTEL_EXPORTER_OTLP_COMPRESSION": "gzip", "OTEL_METRIC_EXPORT_INTERVAL": "1", "OTEL_EXPORTER_OTLP_CERTIFICATE": certificatePath,
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": certificatePath, "OTEL_EXPORTER_OTLP_CLIENT_KEY": keyPath,
	})
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "running"}, func() float64 { return 2 }))

	shutdown, err := Setup(t.Context(), "uuid", "runner", registry)
	require.NoError(t, err)
	_, span := tracer.Start(t.Context(), "op")
	span.End()
	gunzip := func(body []byte) []byte {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		require.NoError(t, err)
		decoded, err := io.ReadAll(reader)
		require.NoError(t, err)
		return decoded
	}
	var metricsData metricpb.MetricsData
	select {
	case body := <-metricBodies:
		require.NoError(t, proto.Unmarshal(gunzip(body), &metricsData))
	case <-time.After(30 * time.Second):
		require.FailNow(t, "no periodic metrics export")
	}
	require.NoError(t, shutdown(t.Context()))

	require.Len(t, traceBodies, 2)
	<-traceBodies
	var tracesData tracepb.TracesData
	require.NoError(t, proto.Unmarshal(gunzip(<-traceBodies), &tracesData))
	assert.Equal(t, 2*time.Second, retryDelay(1, &statusError{"429 Too Many Requests", "2"}, nil))
	assert.Equal(t, "op", tracesData.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0].GetName())

	resourceMetrics := metricsData.GetResourceMetrics()[0]
	assert.True(t, slices.ContainsFunc(resourceMetrics.GetResource().GetAttributes(), func(kv *commonpb.KeyValue) bool {
		return proto.Equal(kv, keyValue("service.name", "gitea-runner"))
	}))
	assert.InDelta(t, 2, resourceMetrics.GetScopeMetrics()[0].GetMetrics()[0].GetGauge().GetDataPoints()[0].GetAsDouble(), 0)
}

func TestConvert(t *testing.T) {
	registry := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "jobs_total", Help: "Jobs."}, []string{"status"})
	histogram := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "duration_seconds", Unit: "seconds", Buckets: []float64{1, 2}})
	summary := prometheus.NewSummary(prometheus.SummaryOpts{Name: "gc_seconds", Objectives: map[float64]float64{0.5: 0.05}})
	registry.MustRegister(counter, histogram, summary)
	counter.WithLabelValues("success").Add(3)
	for _, value := range []float64{0.5, 1.5, 3} {
		histogram.Observe(value)
	}
	summary.Observe(4)
	families, err := registry.Gather()
	require.NoError(t, err)

	exported := convert(families)
	assert.Equal(t, "s", exported[0].GetUnit())
	buckets := exported[0].GetHistogram().GetDataPoints()[0]
	assert.Equal(t, []float64{1, 2}, buckets.GetExplicitBounds())
	assert.Equal(t, []uint64{1, 1, 1}, buckets.GetBucketCounts())
	assert.Equal(t, uint64(3), buckets.GetCount())
	assert.InDelta(t, 5, buckets.GetSum(), 0)
	assert.InDelta(t, 4, exported[1].GetSummary().GetDataPoints()[0].GetQuantileValues()[0].GetValue(), 0)
	sum := exported[2].GetSum()
	assert.Equal(t, "Jobs.", exported[2].GetDescription())
	assert.True(t, proto.Equal(keyValue("prometheus.type", "counter"), exported[2].GetMetadata()[0]))
	assert.NotZero(t, sum.GetDataPoints()[0].GetStartTimeUnixNano())
	assert.True(t, sum.GetIsMonotonic())
	assert.Equal(t, cumulative, sum.GetAggregationTemporality())
	assert.InDelta(t, 3, sum.GetDataPoints()[0].GetAsDouble(), 0)
	assert.True(t, proto.Equal(keyValue("status", "success"), sum.GetDataPoints()[0].GetAttributes()[0]))
}

func TestExportRefusesRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	origin := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer origin.Close()
	require.EqualError(t, (&client{endpoint: origin.URL, headers: http.Header{}, timeout: time.Second, httpClient: exportClient}).UploadTraces(t.Context(), nil), "OTLP export failed: 307 Temporary Redirect")
}

func TestMillis(t *testing.T) {
	for value, want := range map[string]time.Duration{"": time.Minute, "1500": 1500 * time.Millisecond, "0": noLimit, "-1": time.Minute, "1s": time.Minute, "2147483648": time.Minute} {
		t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", value)
		assert.Equal(t, want, millis("OTEL_METRIC_EXPORT_TIMEOUT", time.Minute, noLimit), value)
	}
}

func TestJob(t *testing.T) {
	fields, err := structpb.NewStruct(map[string]any{
		"workflow":         "ci.yml",
		"repository":       "owner/repo",
		"repository_owner": "owner",
		"server_url":       "https://gitea.example/",
		"run_id":           "42",
		"run_number":       "40",
		"run_attempt":      "2",
		"job":              "build",
	})
	require.NoError(t, err)
	task := &runnerv1.Task{Id: 7, Context: fields}

	envs := map[string]string{}
	ctx, endJob := StartJob(t.Context(), task)
	AddJobEnv(ctx, envs)
	stepCtx, endStep := StartStep(ctx, &model.Step{}, "Main")
	endStep(nil, nil)
	endJob(runnerv1.Result_RESULT_SUCCESS)
	assert.Equal(t, ctx, stepCtx)
	assert.Empty(t, envs)

	spans := tracetest.NewSpanRecorder()
	SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { SetTracerProvider(tracenoop.NewTracerProvider()) })

	ctx, endJob = StartJob(t.Context(), task)
	SetJobName(ctx, "Build and test")
	AddJobEnv(ctx, envs)
	stepCtx, endStep = StartStep(ctx, &model.Step{Run: "make\nmake test", Number: 1}, "Main")
	assert.False(t, trace.SpanFromContext(ctx).IsRecording())
	assert.False(t, trace.SpanFromContext(stepCtx).IsRecording())
	endStep(assert.AnError, nil)
	endJob(runnerv1.Result_RESULT_FAILURE)

	jobSpan := trace.SpanContextFromContext(ctx)
	assert.Equal(t, "00-"+jobSpan.TraceID().String()+"-"+jobSpan.SpanID().String()+"-01", envs["TRACEPARENT"])

	ended := spans.Ended()
	require.Len(t, ended, 2)
	step, job := ended[0], ended[1]
	assert.Equal(t, "Run make", step.Name())
	assert.Equal(t, jobSpan.SpanID(), step.Parent().SpanID())
	assert.Equal(t, codes.Error, step.Status().Code)
	assert.Subset(t, step.Attributes(), []any{
		semconv.CICDPipelineTaskRunID("7.1"),
		semconv.CICDPipelineTaskRunURLFull("https://gitea.example/owner/repo/actions/runs/42"),
		semconv.ErrorTypeOther,
	})
	assert.Equal(t, "RUN ci.yml Build and test", job.Name())
	assert.Equal(t, trace.SpanKindServer, job.SpanKind())
	assert.Subset(t, job.Attributes(), []any{
		semconv.VCSOwnerName("owner"),
		semconv.VCSRepositoryName("repo"),
		semconv.CICDPipelineRunID("42"),
		attribute.String("gitea.run.number", "40"),
		attribute.String("gitea.run.attempt", "2"),
		attribute.String("gitea.job.key", "build"),
		attribute.Int64("gitea.task.id", 7),
	})
}

func TestResults(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, expire := context.WithDeadline(t.Context(), time.Time{})
	defer expire()
	for want, tc := range map[string]struct {
		ctx    context.Context
		err    error
		result *model.StepResult
	}{
		"timeout":      {ctx: t.Context(), err: fmt.Errorf("exec: %w", context.DeadlineExceeded)},
		"cancellation": {ctx: cancelled},
		"failure":      {ctx: t.Context(), result: &model.StepResult{Outcome: model.StepStatusFailure, Conclusion: model.StepStatusFailure}},
		"success":      {ctx: t.Context(), result: &model.StepResult{Outcome: model.StepStatusFailure, Conclusion: model.StepStatusSuccess}},
		"skip":         {ctx: t.Context(), result: &model.StepResult{Outcome: model.StepStatusSkipped, Conclusion: model.StepStatusSkipped}},
	} {
		assert.Equal(t, want, stepResult(tc.ctx, tc.err, tc.result))
	}
	assert.Equal(t, "timeout", jobResult(expired, runnerv1.Result_RESULT_FAILURE))
	assert.Equal(t, "cancellation", jobResult(t.Context(), runnerv1.Result_RESULT_CANCELLED))
}
