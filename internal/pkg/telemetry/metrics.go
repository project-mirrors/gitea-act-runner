// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package telemetry

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
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
		attrs = append(attrs, keyValue(string(kv.Key), kv.Value.String()))
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

// convert follows https://opentelemetry.io/docs/specs/otel/compatibility/prometheus_and_openmetrics/#prometheus-metric-points-to-otlp
func convert(families []*dto.MetricFamily) []*metricpb.Metric {
	now := uint64(time.Now().UnixNano())
	metrics := make([]*metricpb.Metric, 0, len(families))
	for _, family := range families {
		metric := &metricpb.Metric{
			Name: family.GetName(), Description: family.GetHelp(), Unit: cmp.Or(ucum[family.GetUnit()], family.GetUnit()),
			Metadata: []*commonpb.KeyValue{keyValue("prometheus.type", strings.ToLower(family.GetType().String()))},
		}
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			sum := &metricpb.Sum{AggregationTemporality: cumulative, IsMonotonic: true}
			for _, series := range family.GetMetric() {
				sum.DataPoints = append(sum.DataPoints, number(series, unixNano(series.GetCounter().GetCreatedTimestamp()), now, series.GetCounter().GetValue()))
			}
			metric.Data = &metricpb.Metric_Sum{Sum: sum}
		case dto.MetricType_GAUGE:
			gauge := &metricpb.Gauge{}
			for _, series := range family.GetMetric() {
				gauge.DataPoints = append(gauge.DataPoints, number(series, 0, now, series.GetGauge().GetValue()))
			}
			metric.Data = &metricpb.Metric_Gauge{Gauge: gauge}
		case dto.MetricType_HISTOGRAM:
			histogram := &metricpb.Histogram{AggregationTemporality: cumulative}
			for _, series := range family.GetMetric() {
				histogram.DataPoints = append(histogram.DataPoints, histogramPoint(series, now))
			}
			metric.Data = &metricpb.Metric_Histogram{Histogram: histogram}
		case dto.MetricType_SUMMARY:
			summary := &metricpb.Summary{}
			for _, series := range family.GetMetric() {
				point := &metricpb.SummaryDataPoint{
					Attributes: labels(series), StartTimeUnixNano: unixNano(series.GetSummary().GetCreatedTimestamp()), TimeUnixNano: now,
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
	return metrics
}

func number(series *dto.Metric, start, now uint64, value float64) *metricpb.NumberDataPoint {
	return &metricpb.NumberDataPoint{Attributes: labels(series), StartTimeUnixNano: start, TimeUnixNano: now, Value: &metricpb.NumberDataPoint_AsDouble{AsDouble: value}}
}

// unixNano maps a nil timestamp to 0, leaving the start time unset as the spec asks.
func unixNano(timestamp *timestamppb.Timestamp) uint64 {
	return uint64(timestamp.AsTime().UnixNano())
}

func histogramPoint(series *dto.Metric, now uint64) *metricpb.HistogramDataPoint {
	histogram := series.GetHistogram()
	point := &metricpb.HistogramDataPoint{
		Attributes: labels(series), StartTimeUnixNano: unixNano(histogram.GetCreatedTimestamp()), TimeUnixNano: now,
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

func labels(series *dto.Metric) []*commonpb.KeyValue {
	attrs := make([]*commonpb.KeyValue, 0, len(series.GetLabel()))
	for _, label := range series.GetLabel() {
		attrs = append(attrs, keyValue(label.GetName(), label.GetValue()))
	}
	return attrs
}

func keyValue(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}
