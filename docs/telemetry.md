# OpenTelemetry

The runner can send a trace of every job it runs, and its metrics, to an OpenTelemetry collector or any backend that accepts OTLP. Each job becomes a trace with a span per step, so you can see where CI time goes and which steps fail. Export stays off until you set an endpoint.

## Setup

Set the endpoint in the runner's environment, for example in its systemd unit, with `docker run -e` or in the Kubernetes pod spec:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
```

| Variable | Purpose |
| --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector base URL, traces go to `/v1/traces` and metrics to `/v1/metrics` under it |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Full URL per signal, takes precedence over the base URL |
| `OTEL_EXPORTER_OTLP_HEADERS` | Request headers such as an API key, as comma-separated `key=value` pairs with URL-encoded values |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | Export timeout in milliseconds, default 10000, 0 for none |
| `OTEL_EXPORTER_OTLP_COMPRESSION` | `gzip` or `none`, default `none` |
| `OTEL_EXPORTER_OTLP_CERTIFICATE` | PEM file of the CA to trust instead of the system ones |
| `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE`, `OTEL_EXPORTER_OTLP_CLIENT_KEY` | PEM files of the client certificate and key for mTLS |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Resource attributes, the service name defaults to `gitea-runner` |
| `OTEL_TRACES_SAMPLER`, `OTEL_BSP_*` | Sampling and batching, see the [SDK variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/) |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT` | Metrics export interval and timeout in milliseconds, default 60000 and 30000, a timeout of 0 for none |
| `OTEL_SDK_DISABLED=true` | Turn all export off |
| `OTEL_TRACES_EXPORTER=none`, `OTEL_METRICS_EXPORTER=none` | Turn traces or metrics off |

Except for the endpoint, each `OTEL_EXPORTER_OTLP_*` variable has `TRACES` and `METRICS` variants such as `OTEL_EXPORTER_OTLP_METRICS_HEADERS`, which take precedence for that signal. Only the `otlp` exporter over the `http/protobuf` protocol is supported.

## What is sent

- A span per job, named `RUN {workflow} {job}`, with the repository, ref, commit, run URL, run number, attempt and result.
- A span per step, named as in Gitea's step list, including `Pre` and `Post` stages and the steps of composite actions, with the step's result.
- The runner's name, UUID and version.

Logs, step output and secrets are never sent. Step names are taken from the workflow file as written, without expanding `${{ }}` expressions.

## Tracing inside jobs

Jobs receive `TRACEPARENT`, so tools in a job that use OpenTelemetry can add their spans to the job's trace. To let them export, set their `OTEL_*` variables in `runner.envs`, see [Security](#security).

## Metrics

The metrics of the Prometheus endpoint are also exported, under the same names and whether or not `metrics.enabled` is set. They are sent every export interval and once more on shutdown. If your collector only accepts traces, set `OTEL_METRICS_EXPORTER=none`.

## Security

- OTLP has no authentication of its own. Send to a collector on the runner's host that holds the backend credentials, as the [OpenTelemetry security guide](https://opentelemetry.io/docs/security/config-best-practices/) recommends, or use `https` with a token in `OTEL_EXPORTER_OTLP_HEADERS` or a client certificate. Over plain `http`, headers travel unencrypted.
- TLS certificates are verified against the system CA bundle, or only against `OTEL_EXPORTER_OTLP_CERTIFICATE` when set. Redirects are not followed, so headers only reach the configured endpoint.
- Jobs in containers never see the runner's `OTEL_*` variables. Jobs on the host inherit the runner's whole environment, headers included, and can read its files anyway since they run as the same user.
- Every workflow can read `runner.envs`, so never put credentials there. Point jobs at a collector that holds them instead, keeping in mind that any workflow can then send data through it.
- Traces carry the repository and step names of every job, private repositories included, so give the collector the same access controls as the runner's logs.
