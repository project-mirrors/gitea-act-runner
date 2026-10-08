// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package telemetry

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/internal/pkg/ver"

	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"github.com/avast/retry-go/v5"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

const scope = "gitea.com/gitea/runner"

var processCreation = time.Now() // package init, within milliseconds of the process start

type taskRunKey struct{}

type taskRun struct {
	id, url, pipeline string
	span              trace.Span
}

// tracer stays off the global provider and ctx carries only span contexts, so the Docker client's otelhttp records nothing.
var tracer trace.Tracer = tracenoop.Tracer{}

func SetTracerProvider(provider trace.TracerProvider) {
	tracer = provider.Tracer(scope)
}

func Setup(ctx context.Context, uuid, name string, gatherer prometheus.Gatherer) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return noop, nil
	}
	clients := map[string]*client{}
	for _, signal := range []string{"TRACES", "METRICS"} {
		endpoint := os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT")
		if base := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint == "" && base != "" {
			endpoint = strings.TrimSuffix(base, "/") + "/v1/" + strings.ToLower(signal)
		}
		if endpoint == "" || !exportsOTLP(signal) {
			continue
		}
		if protocol := os.Getenv(otlpKey(signal, "PROTOCOL")); protocol != "" && !strings.EqualFold(protocol, "http/protobuf") {
			return noop, fmt.Errorf("OTLP protocol %q is not supported, only http/protobuf is", protocol)
		}
		headers := http.Header{}
		for _, key := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_" + signal + "_HEADERS"} {
			for pair := range strings.SplitSeq(os.Getenv(key), ",") {
				key, value, found := strings.Cut(pair, "=")
				if decoded, err := url.PathUnescape(strings.TrimSpace(value)); found && err == nil {
					headers.Set(strings.TrimSpace(key), decoded)
				}
			}
		}
		compressionKey := otlpKey(signal, "COMPRESSION")
		compression := os.Getenv(compressionKey)
		if !slices.Contains([]string{"", "none", "gzip"}, strings.ToLower(compression)) {
			log.Warnf("ignoring unsupported %s=%q", compressionKey, compression)
		}
		httpClient, err := newHTTPClient(signal)
		if err != nil {
			return noop, err
		}
		clients[signal] = &client{
			endpoint: endpoint, headers: headers, timeout: millis(otlpKey(signal, "TIMEOUT"), 10*time.Second, noLimit),
			gzip: strings.EqualFold(compression, "gzip"), httpClient: httpClient,
		}
	}
	if len(clients) == 0 {
		return noop, nil
	}
	res, err := resource.New(ctx, resource.WithTelemetrySDK(), resource.WithProcessPID(), resource.WithProcessRuntimeName(), resource.WithProcessRuntimeVersion(), resource.WithAttributes(
		semconv.ServiceName("gitea-runner"),
		semconv.ServiceVersion(ver.Version()),
		semconv.ServiceInstanceID(uuid),
		semconv.CICDWorkerID(uuid),
		semconv.CICDWorkerName(name),
		semconv.ProcessCreationTime(processCreation.UTC().Format(time.RFC3339Nano)),
	), resource.WithFromEnv())
	if err != nil {
		return noop, err
	}
	var shutdown []func(context.Context) error
	if c := clients["TRACES"]; c != nil {
		provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(otlptrace.NewUnstarted(c)), sdktrace.WithResource(res))
		SetTracerProvider(provider)
		shutdown = append(shutdown, provider.Shutdown)
	}
	if c := clients["METRICS"]; c != nil {
		shutdown = append(shutdown, pushMetrics(c, res, gatherer))
	}
	return func(ctx context.Context) error {
		var group errgroup.Group
		for _, fn := range shutdown {
			group.Go(func() error { return fn(ctx) })
		}
		return group.Wait()
	}, nil
}

func exportsOTLP(signal string) bool {
	key := "OTEL_" + signal + "_EXPORTER"
	otlp, known := false, false
	for value := range strings.SplitSeq(os.Getenv(key), ",") {
		switch value = strings.TrimSpace(value); strings.ToLower(value) {
		case "":
		case "otlp":
			otlp, known = true, true
		case "none":
			known = true
		default:
			log.Warnf("ignoring unsupported %s value %q", key, value)
		}
	}
	return otlp || !known
}

func otlpKey(signal, name string) string {
	if key := "OTEL_EXPORTER_OTLP_" + signal + "_" + name; os.Getenv(key) != "" {
		return key
	}
	return "OTEL_EXPORTER_OTLP_" + name
}

const noLimit = math.MaxInt32 * time.Millisecond // the spec's stand-in for a 0 timeout

func millis(key string, fallback, zero time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	ms, err := strconv.ParseInt(value, 10, 32)
	if err != nil || ms < 0 {
		log.Warnf("ignoring %s=%q, it must be a non-negative number of milliseconds", key, value)
		return fallback
	}
	return cmp.Or(time.Duration(ms)*time.Millisecond, zero)
}

// newHTTPClient applies the OTLP certificate variables, a configured CA replaces the system roots like in the Go SDK.
func newHTTPClient(signal string) (*http.Client, error) {
	caFile, certFile, keyFile := os.Getenv(otlpKey(signal, "CERTIFICATE")), os.Getenv(otlpKey(signal, "CLIENT_CERTIFICATE")), os.Getenv(otlpKey(signal, "CLIENT_KEY"))
	if caFile == "" && certFile == "" && keyFile == "" {
		return exportClient, nil
	}
	config, err := tlsconfig.Client(tlsconfig.Options{CAFile: caFile, CertFile: certFile, KeyFile: keyFile, ExclusiveRootPools: true})
	if err != nil {
		return nil, err
	}
	transport, _ := http.DefaultTransport.(*http.Transport)
	transport = transport.Clone()
	transport.TLSClientConfig = config
	return &http.Client{Transport: transport, CheckRedirect: exportClient.CheckRedirect}, nil
}

// client replaces the otlp*http exporters, which link gRPC (+7 MB): https://github.com/open-telemetry/opentelemetry-go/issues/2579
type client struct {
	endpoint   string
	headers    http.Header
	timeout    time.Duration
	gzip       bool
	httpClient *http.Client
}

var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// exportClient refuses redirects, which would carry the collector's header credentials to another host.
var exportClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (*client) Start(context.Context) error { return nil }

func (*client) Stop(context.Context) error { return nil }

func (c *client) UploadTraces(ctx context.Context, spans []*tracepb.ResourceSpans) error {
	return c.upload(ctx, &tracepb.TracesData{ResourceSpans: spans})
}

func (c *client) upload(ctx context.Context, data proto.Message) error {
	body, err := proto.Marshal(data)
	if err != nil {
		return err
	}
	if c.gzip {
		var buffer bytes.Buffer
		writer, _ := gzipWriters.Get().(*gzip.Writer)
		writer.Reset(&buffer)
		_, _ = writer.Write(body)
		_ = writer.Close()
		gzipWriters.Put(writer)
		body = buffer.Bytes()
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return retry.New(retry.Context(ctx), retry.Attempts(5), retry.LastErrorOnly(true), retry.DelayType(retryDelay)).Do(func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return retry.Unrecoverable(err)
		}
		req.Header = c.headers.Clone()
		req.Header.Set("Content-Type", "application/x-protobuf")
		if c.gzip {
			req.Header.Set("Content-Encoding", "gzip")
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode < http.StatusMultipleChoices {
			return nil
		}
		err = &statusError{resp.Status, resp.Header.Get("Retry-After")}
		if slices.Contains([]int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}, resp.StatusCode) {
			return err
		}
		return retry.Unrecoverable(err)
	})
}

type statusError struct{ status, retryAfter string }

func (e *statusError) Error() string { return "OTLP export failed: " + e.status }

func retryDelay(attempt uint, err error, config retry.DelayContext) time.Duration {
	if status := (*statusError)(nil); errors.As(err, &status) {
		if seconds, err := strconv.ParseUint(status.retryAfter, 10, 31); err == nil {
			return time.Duration(seconds) * time.Second
		}
	}
	return retry.CombineDelay(retry.BackOffDelay, retry.RandomDelay)(attempt, err, config)
}

func StartJob(ctx context.Context, task *runnerv1.Task) (context.Context, func(runnerv1.Result)) {
	field := func(key string) string { return task.GetContext().GetFields()[key].GetStringValue() }
	pipeline, owner, repo := field("workflow"), field("repository_owner"), field("repository")
	repoURL := strings.TrimSuffix(field("server_url"), "/") + "/" + repo
	runURL := repoURL + "/actions/runs/" + field("run_id")
	_, span := tracer.Start(ctx, "RUN "+pipeline, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
		semconv.CICDPipelineName(pipeline),
		semconv.CICDPipelineActionNameRun,
		semconv.CICDPipelineRunID(field("run_id")),
		semconv.CICDPipelineRunURLFull(runURL),
		semconv.VCSOwnerName(owner),
		semconv.VCSRepositoryName(strings.TrimPrefix(repo, owner+"/")),
		semconv.VCSRepositoryURLFull(repoURL),
		semconv.VCSRefHeadName(field("ref_name")),
		semconv.VCSRefHeadRevision(field("sha")),
		attribute.String("gitea.job.key", field("job")),
		attribute.String("gitea.run.number", field("run_number")),
		attribute.String("gitea.run.attempt", field("run_attempt")),
		attribute.Int64("gitea.task.id", task.Id),
	))
	if !span.SpanContext().IsValid() {
		return ctx, func(runnerv1.Result) {}
	}
	ctx = context.WithValue(trace.ContextWithSpanContext(ctx, span.SpanContext()), taskRunKey{}, taskRun{strconv.FormatInt(task.Id, 10), runURL, pipeline, span})
	return ctx, func(result runnerv1.Result) {
		end(span, semconv.CICDPipelineResultKey, jobResult(ctx, result))
	}
}

func SetJobName(ctx context.Context, job string) {
	if run, ok := ctx.Value(taskRunKey{}).(taskRun); ok {
		run.span.SetName("RUN " + run.pipeline + " " + job)
	}
}

func AddJobEnv(ctx context.Context, envs map[string]string) {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	for key, value := range carrier {
		envs[strings.ToUpper(key)] = value
	}
}

func StartStep(ctx context.Context, step *model.Step, stage string) (context.Context, func(error, *model.StepResult)) {
	parent, ok := ctx.Value(taskRunKey{}).(taskRun)
	if !ok {
		return ctx, func(error, *model.StepResult) {}
	}
	name, id := step.Name, parent.id+"."+strconv.Itoa(step.Number)
	if name == "" {
		run, _, _ := strings.Cut(strings.TrimSpace(step.Run), "\n")
		name = "Run " + cmp.Or(strings.TrimSpace(run), step.String())
	}
	if stage != "Main" {
		name, id = stage+" "+name, id+"."+strings.ToLower(stage)
	}
	_, span := tracer.Start(ctx, name, trace.WithAttributes(
		semconv.CICDPipelineTaskName(name),
		semconv.CICDPipelineTaskRunID(id),
		semconv.CICDPipelineTaskRunURLFull(parent.url),
	))
	ctx = context.WithValue(trace.ContextWithSpanContext(ctx, span.SpanContext()), taskRunKey{}, taskRun{id, parent.url, parent.pipeline, span})
	return ctx, func(err error, result *model.StepResult) {
		end(span, semconv.CICDPipelineTaskRunResultKey, stepResult(ctx, err, result))
	}
}

func jobResult(ctx context.Context, result runnerv1.Result) string {
	switch {
	case result == runnerv1.Result_RESULT_SUCCESS:
		return "success"
	case result == runnerv1.Result_RESULT_SKIPPED:
		return "skip"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case result == runnerv1.Result_RESULT_CANCELLED:
		return "cancellation"
	case result == runnerv1.Result_RESULT_FAILURE:
		return "failure"
	}
	return "error"
}

func stepResult(ctx context.Context, err error, result *model.StepResult) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return "cancellation"
	case err != nil || result.Conclusion == model.StepStatusFailure:
		return "failure"
	case result.Conclusion == model.StepStatusSkipped:
		return "skip"
	}
	return "success"
}

func end(span trace.Span, key attribute.Key, result string) {
	span.SetAttributes(key.String(result))
	switch result {
	case "timeout":
		span.SetAttributes(semconv.ErrorTypeKey.String(result))
		span.SetStatus(codes.Error, "")
	case "failure", "error":
		span.SetAttributes(semconv.ErrorTypeOther)
		span.SetStatus(codes.Error, "")
	}
	span.End()
}
