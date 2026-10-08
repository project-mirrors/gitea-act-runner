// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package telemetry

import (
	"cmp"
	"context"
	"errors"
	"math"
	"runtime"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/pkg/metrics"

	"gitea.dev/actionslib/runner/v1/runnerv1connect"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const cumulative = metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE

// pushMetrics also pushes on shutdown so short-lived runners report.
func pushMetrics(exporter *client, res *resource.Resource, gatherer prometheus.Gatherer) func(context.Context) error {
	attrs := make([]*commonpb.KeyValue, 0, res.Len())
	for _, kv := range res.Attributes() {
		attr := keyValue(string(kv.Key), kv.Value.String())
		if kv.Value.Type() == attribute.INT64 {
			attr.Value.Value = &commonpb.AnyValue_IntValue{IntValue: kv.Value.AsInt64()}
		}
		attrs = append(attrs, attr)
	}
	interval, timeout := millis("OTEL_METRIC_EXPORT_INTERVAL", time.Minute, time.Minute), millis("OTEL_METRIC_EXPORT_TIMEOUT", 30*time.Second, noLimit)
	push := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		families, err := gatherer.Gather()
		return errors.Join(err, exporter.upload(ctx, &metricpb.MetricsData{ResourceMetrics: []*metricpb.ResourceMetrics{{
			Resource:     &resourcepb.Resource{Attributes: attrs},
			ScopeMetrics: []*metricpb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: scope}, Metrics: convert(families)}},
			SchemaUrl:    res.SchemaURL(),
		}}}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := push(ctx); err != nil && ctx.Err() == nil {
					log.WithError(err).Warn("OTLP metrics export failed, set OTEL_METRICS_EXPORTER=none to disable it")
				}
			}
		}
	}()
	return func(ctx context.Context) error {
		cancel()
		<-done
		return push(ctx)
	}
}

var ucum = map[string]string{
	"days": "d", "hours": "h", "minutes": "min", "seconds": "s", "milliseconds": "ms", "microseconds": "us", "nanoseconds": "ns",
	"bytes": "By", "kibibytes": "KiBy", "mebibytes": "MiBy", "gibibytes": "GiBy", "tebibytes": "TiBy",
	"kilobytes": "kBy", "megabytes": "MBy", "gigabytes": "GBy", "terabytes": "TBy",
	"meters": "m", "volts": "V", "amperes": "A", "joules": "J", "watts": "W", "grams": "g", "celsius": "Cel", "hertz": "Hz", "percent": "%",
}

type otlpMetric struct {
	name, unit string
	upDown     bool              // a gauge semconv defines as an UpDownCounter
	labels     map[string]string // label → attribute
	values     map[string]string // label value → attribute value
	extra      []*commonpb.KeyValue
	unlimited  float64 // gauge value meaning no limit, not sent
}

var rpcMethods = map[string]string{
	metrics.LabelMethodFetchTask:  strings.TrimPrefix(runnerv1connect.RunnerServiceFetchTaskProcedure, "/"),
	metrics.LabelMethodUpdateLog:  strings.TrimPrefix(runnerv1connect.RunnerServiceUpdateLogProcedure, "/"),
	metrics.LabelMethodUpdateTask: strings.TrimPrefix(runnerv1connect.RunnerServiceUpdateTaskProcedure, "/"),
}

// openFiles maps process_open_fds, which client_golang fills with the handle count on Windows.
func openFiles() otlpMetric {
	if runtime.GOOS == "windows" {
		return otlpMetric{name: "process.windows.handle.count", unit: "{handle}", upDown: true}
	}
	return otlpMetric{name: "process.unix.file_descriptor.count", unit: "{file_descriptor}", upDown: true}
}

// otlpMetrics maps to semconv names, unlisted metrics keep theirs and an empty name drops one the resource already carries.
var otlpMetrics = map[string]otlpMetric{
	"gitea_runner_info":                           {},
	"go_info":                                     {},
	"process_start_time_seconds":                  {},
	"gitea_runner_capacity":                       {name: "gitea.runner.job.limit", unit: "{job}", upDown: true},
	"gitea_runner_uptime_seconds":                 {name: "gitea.runner.uptime", unit: "s"},
	"gitea_runner_job_running":                    {name: "gitea.runner.job.active", unit: "{job}", upDown: true},
	"gitea_runner_job_capacity_utilization_ratio": {name: "gitea.runner.job.utilization", unit: "1"},
	"gitea_runner_job_duration_seconds":           {name: "gitea.runner.job.duration", unit: "s"},
	"gitea_runner_job_total": {
		name: "gitea.runner.jobs", unit: "{job}",
		labels: map[string]string{"status": "cicd.pipeline.result"},
		values: map[string]string{metrics.LabelStatusCancelled: "cancellation", metrics.LabelStatusSkipped: "skip", metrics.LabelStatusUnknown: "error"},
	},
	"gitea_runner_poll_fetch_total": {
		name: "gitea.runner.poll.fetches", unit: "{fetch}",
		labels: map[string]string{"result": "gitea.runner.poll.result"},
	},
	"gitea_runner_poll_fetch_duration_seconds":   {name: "gitea.runner.poll.fetch.duration", unit: "s"},
	"gitea_runner_poll_backoff_seconds":          {name: "gitea.runner.poll.backoff", unit: "s"},
	"gitea_runner_report_log_duration_seconds":   {name: "gitea.runner.report.log.duration", unit: "s"},
	"gitea_runner_report_state_duration_seconds": {name: "gitea.runner.report.state.duration", unit: "s"},
	"gitea_runner_report_log_buffer_rows":        {name: "gitea.runner.report.log.buffer", unit: "{row}", upDown: true},
	"gitea_runner_report_log_total": {
		name: "gitea.runner.report.log.requests", unit: "{request}",
		labels: map[string]string{"result": "gitea.runner.report.result"},
	},
	"gitea_runner_report_state_total": {
		name: "gitea.runner.report.state.requests", unit: "{request}",
		labels: map[string]string{"result": "gitea.runner.report.result"},
	},
	"gitea_runner_client_errors_total": {
		name: "cicd.system.errors", unit: "{error}",
		labels: map[string]string{"method": "rpc.method", "code": "error.type"}, values: rpcMethods,
		extra: []*commonpb.KeyValue{keyValue("cicd.system.component", "runner")},
	},
	"gitea_runner_state": {
		name: "cicd.worker.count", unit: "{worker}", upDown: true,
		labels: map[string]string{"state": "cicd.worker.state"},
		values: map[string]string{metrics.LabelStateIdle: "available", metrics.LabelStateUnavailable: "offline"},
	},
	"go_goroutines":                 {name: "go.goroutine.count", unit: "{goroutine}", upDown: true},
	"go_memstats_alloc_bytes_total": {name: "go.memory.allocated", unit: "By"},
	"go_memstats_next_gc_bytes":     {name: "go.memory.gc.goal", unit: "By", upDown: true},
	"go_gc_gomemlimit_bytes":        {name: "go.memory.limit", unit: "By", upDown: true, unlimited: math.MaxInt64},
	"go_gc_gogc_percent":            {name: "go.config.gogc", unit: "%", upDown: true},
	"go_sched_gomaxprocs_threads":   {name: "go.processor.limit", unit: "{thread}", upDown: true},
	"process_resident_memory_bytes": {name: "process.memory.usage", unit: "By", upDown: true},
	"process_virtual_memory_bytes":  {name: "process.memory.virtual", unit: "By", upDown: true},
	"process_open_fds":              openFiles(),
}

// convert follows https://opentelemetry.io/docs/specs/otel/compatibility/prometheus_and_openmetrics/#prometheus-metric-points-to-otlp except for otlpMetrics.
func convert(families []*dto.MetricFamily) []*metricpb.Metric {
	now := uint64(time.Now().UnixNano())
	metrics := make([]*metricpb.Metric, 0, len(families))
	byFamily := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byFamily[family.GetName()] = family
		mapping, ok := otlpMetrics[family.GetName()]
		if !ok {
			mapping.name = family.GetName()
		} else if mapping.name == "" {
			continue
		}
		metric := &metricpb.Metric{
			Name: mapping.name, Description: family.GetHelp(), Unit: cmp.Or(mapping.unit, ucum[family.GetUnit()], family.GetUnit()),
			Metadata: []*commonpb.KeyValue{keyValue("prometheus.type", strings.ToLower(family.GetType().String()))},
		}
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			sum := &metricpb.Sum{AggregationTemporality: cumulative, IsMonotonic: true}
			for _, series := range family.GetMetric() {
				sum.DataPoints = append(sum.DataPoints, number(labels(series, mapping), unixNano(series.GetCounter().GetCreatedTimestamp()), now, series.GetCounter().GetValue()))
			}
			metric.Data = &metricpb.Metric_Sum{Sum: sum}
		case dto.MetricType_GAUGE:
			var points []*metricpb.NumberDataPoint
			for _, series := range family.GetMetric() {
				if value := series.GetGauge().GetValue(); mapping.unlimited == 0 || value != mapping.unlimited {
					points = append(points, number(labels(series, mapping), 0, now, value))
				}
			}
			if len(points) == 0 {
				continue
			}
			if mapping.upDown {
				metric.Data = &metricpb.Metric_Sum{Sum: &metricpb.Sum{AggregationTemporality: cumulative, DataPoints: points}}
			} else {
				metric.Data = &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: points}}
			}
		case dto.MetricType_HISTOGRAM:
			histogram := &metricpb.Histogram{AggregationTemporality: cumulative}
			for _, series := range family.GetMetric() {
				histogram.DataPoints = append(histogram.DataPoints, histogramPoint(series, labels(series, mapping), now))
			}
			metric.Data = &metricpb.Metric_Histogram{Histogram: histogram}
		case dto.MetricType_SUMMARY:
			summary := &metricpb.Summary{}
			for _, series := range family.GetMetric() {
				point := &metricpb.SummaryDataPoint{
					Attributes: labels(series, mapping), StartTimeUnixNano: unixNano(series.GetSummary().GetCreatedTimestamp()), TimeUnixNano: now,
					Count: series.GetSummary().GetSampleCount(), Sum: series.GetSummary().GetSampleSum(),
				}
				for _, quantile := range series.GetSummary().GetQuantile() {
					point.QuantileValues = append(point.QuantileValues, &metricpb.SummaryDataPoint_ValueAtQuantile{Quantile: quantile.GetQuantile(), Value: quantile.GetValue()})
				}
				summary.DataPoints = append(summary.DataPoints, point)
			}
			metric.Data = &metricpb.Metric_Summary{Summary: summary}
		default:
			continue
		}
		metrics = append(metrics, metric)
	}
	return append(metrics, derived(byFamily, now)...)
}

// derived adds the semconv Go metrics that are computed from other metrics rather than renamed.
func derived(families map[string]*dto.MetricFamily, now uint64) []*metricpb.Metric {
	first := func(name string) *dto.Metric {
		if series := families[name].GetMetric(); len(series) > 0 {
			return series[0]
		}
		return nil
	}
	gauge := func(name string) (float64, bool) {
		series := first(name)
		return series.GetGauge().GetValue(), series != nil
	}
	sum := func(name, unit, description string, monotonic bool, points ...*metricpb.NumberDataPoint) *metricpb.Metric {
		return &metricpb.Metric{Name: name, Unit: unit, Description: description, Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{
			AggregationTemporality: cumulative, IsMonotonic: monotonic, DataPoints: points,
		}}}
	}
	var metrics []*metricpb.Metric
	total, ok1 := gauge("go_memstats_sys_bytes")
	released, ok2 := gauge("go_memstats_heap_released_bytes")
	stack, ok3 := gauge("go_memstats_stack_inuse_bytes")
	if ok1 && ok2 && ok3 {
		metrics = append(metrics, sum("go.memory.used", "By", "Memory used by the Go runtime.", false,
			number([]*commonpb.KeyValue{keyValue("go.memory.type", "stack")}, 0, now, stack),
			number([]*commonpb.KeyValue{keyValue("go.memory.type", "other")}, 0, now, total-released-stack)))
	}
	if gc := first("go_gc_duration_seconds"); gc != nil {
		metrics = append(metrics, sum("go.memory.gc.cycles", "{gc_cycle}", "Number of completed GC cycles.", true,
			number(nil, unixNano(gc.GetSummary().GetCreatedTimestamp()), now, float64(gc.GetSummary().GetSampleCount()))))
	}
	return metrics
}

func number(attrs []*commonpb.KeyValue, start, now uint64, value float64) *metricpb.NumberDataPoint {
	return &metricpb.NumberDataPoint{Attributes: attrs, StartTimeUnixNano: start, TimeUnixNano: now, Value: &metricpb.NumberDataPoint_AsDouble{AsDouble: value}}
}

// unixNano maps a nil timestamp to 0, leaving the start time unset as the spec asks.
func unixNano(timestamp *timestamppb.Timestamp) uint64 {
	return uint64(timestamp.AsTime().UnixNano())
}

func histogramPoint(series *dto.Metric, attrs []*commonpb.KeyValue, now uint64) *metricpb.HistogramDataPoint {
	histogram := series.GetHistogram()
	point := &metricpb.HistogramDataPoint{
		Attributes: attrs, StartTimeUnixNano: unixNano(histogram.GetCreatedTimestamp()), TimeUnixNano: now,
		Count: histogram.GetSampleCount(), Sum: histogram.SampleSum,
		BucketCounts: make([]uint64, len(histogram.GetBucket())+1),
	}
	var below uint64
	for i, bucket := range histogram.GetBucket() {
		point.ExplicitBounds = append(point.ExplicitBounds, bucket.GetUpperBound())
		point.BucketCounts[i], below = bucket.GetCumulativeCount()-below, bucket.GetCumulativeCount()
	}
	point.BucketCounts[len(histogram.GetBucket())] = histogram.GetSampleCount() - below
	return point
}

func labels(series *dto.Metric, mapping otlpMetric) []*commonpb.KeyValue {
	attrs := make([]*commonpb.KeyValue, 0, len(series.GetLabel())+len(mapping.extra))
	for _, label := range series.GetLabel() {
		attrs = append(attrs, keyValue(cmp.Or(mapping.labels[label.GetName()], label.GetName()), cmp.Or(mapping.values[label.GetValue()], label.GetValue())))
	}
	return append(attrs, mapping.extra...)
}

func keyValue(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}
