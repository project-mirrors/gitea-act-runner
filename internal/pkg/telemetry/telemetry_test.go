// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package telemetry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func setEnv(t *testing.T, env map[string]string) {
	t.Cleanup(func() { SetTracerProvider(tracenoop.NewTracerProvider()) })
	for _, key := range []string{
		"OTEL_SDK_DISABLED", "OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	} {
		t.Setenv(key, env[key])
	}
}

func TestSetup(t *testing.T) {
	for _, tc := range []struct {
		env     map[string]string
		enabled bool
		err     string
	}{
		{},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"}, enabled: true},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_TRACES_EXPORTER": "NONE"}},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_SDK_DISABLED": "TRUE"}},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, err: `OTLP protocol "grpc" is not supported, only http/protobuf is`},
	} {
		t.Run(fmt.Sprint(tc.env), func(t *testing.T) {
			setEnv(t, tc.env)
			shutdown, err := Setup(t.Context(), "uuid", "runner")
			if tc.err != "" {
				require.EqualError(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			_, disabled := tracer.(tracenoop.Tracer)
			assert.Equal(t, tc.enabled, !disabled)
			require.NoError(t, shutdown(t.Context()))
		})
	}
}

func TestExportRetriesAndMatchesOTLP(t *testing.T) {
	bodies := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/otlp/v1/traces", r.URL.Path)
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	setEnv(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": server.URL + "/otlp/", "OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20token"})

	shutdown, err := Setup(t.Context(), "uuid", "runner")
	require.NoError(t, err)
	_, span := tracer.Start(t.Context(), "op")
	span.End()
	require.NoError(t, shutdown(t.Context()))

	require.Len(t, bodies, 2)
	<-bodies
	var decoded tracepb.TracesData
	require.NoError(t, proto.Unmarshal(<-bodies, &decoded))
	assert.Equal(t, "op", decoded.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0].GetName())
}

func TestExportRefusesRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	origin := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer origin.Close()
	require.EqualError(t, (&client{origin.URL, http.Header{}}).UploadTraces(t.Context(), nil), "OTLP export failed: 307 Temporary Redirect")
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
