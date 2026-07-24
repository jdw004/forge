# Metrics and traces

Forge uses a separate Prometheus registry per process. API and demo modes serve
`/metrics` on their normal listener. If `FORGE_API_TOKEN` is set, scraping requires
the same bearer token as the API.

```sh
curl http://127.0.0.1:8080/metrics
# With authentication:
curl -H "Authorization: Bearer $FORGE_API_TOKEN" http://127.0.0.1:8080/metrics
```

An independent worker can enable an optional metrics listener:

```sh
FORGE_METRICS_ADDR=127.0.0.1:9091 go run ./cmd/forge -mode worker
```

It exposes only `/metrics`, `/healthz`, and `/readyz`, with no workflow routes.
`DATABASE_URL` is still required. Remote binding requires an API token of at least
32 bytes. Use unique ports for workers sharing a host, or container networks.

## Metric meanings

| Metric | Meaning |
| --- | --- |
| `forge_worker_capacity` | Configured slots in this process; not throughput |
| `forge_task_active` | Executing attempts, including completion acknowledgement |
| `forge_task_attempts_total{state,reason,committed}` | Ended attempts, including abandoned results |
| `forge_tasks_completed_total{state}` | Completions accepted by the store |
| `forge_task_tokens_total` | Simulated tokens from all attempts |
| `forge_breaker_trips_total{reason}` | Observed token/runtime breaker trips |
| `forge_task_attempt_seconds` | Attempt duration including acknowledgement |
| `forge_http_requests_total{method,route,status}` | Requests grouped by route template |
| `forge_http_request_seconds{method,route}` | HTTP request duration |

Go runtime metrics are also included. Metric scrapes are excluded from request
metrics and traces. Counters reset on process restart. Retries are new attempts,
so token and breaker counts can include work whose result was never accepted.
Skipped tasks and direct store cancellations do not increment worker completion
counters; workflow details are the source of truth for persisted states.

Scrape each independent worker for a distributed view. API metrics do not
aggregate worker processes. Example PromQL:

```promql
sum(forge_task_active)
sum(rate(forge_tasks_completed_total{state="succeeded"}[5m]))
sum by (reason) (rate(forge_breaker_trips_total[5m]))
histogram_quantile(0.95, sum by (le) (rate(forge_task_attempt_seconds_bucket[5m])))
```

The [Prometheus Go client](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus)
uses fixed labels and route templates. Task IDs, workflow IDs, bearer tokens,
query strings, and raw request paths never become metric labels.

## Optional OpenTelemetry tracing

An empty `OTEL_EXPORTER_OTLP_ENDPOINT` disables tracing with no exporter network
calls; this is the default. To use an existing OTLP/HTTP collector, set a base URL.
Forge appends `/v1/traces`:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 \
FORGE_TRACE_SAMPLE_RATIO=1 \
go run ./cmd/forge -mode demo
```

| Setting | Default | Purpose |
| --- | --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Empty | Collector base URL |
| `OTEL_SERVICE_NAME` | `forge-<mode>` | Trace service name |
| `FORGE_TRACE_SAMPLE_RATIO` | `0.1` | Root sampling ratio, between 0 and 1 |

Use HTTPS for remote collectors. Standard OTLP exporter header environment
variables can supply credentials; do not put credentials in the URL. The
parent-based sampler honors incoming W3C `traceparent` sampling decisions.
Baggage, authorization headers, and request bodies are not recorded.

Server spans use route templates. Task spans include workflow/task/worker IDs,
attempt number, tokens, outcome, and whether the result was committed. The queue
does not persist request trace context, so task spans are independent roots;
correlate them by workflow/task IDs. There is no automatic parentage across the
PostgreSQL queue boundary.

The [OpenTelemetry Go SDK](https://opentelemetry.io/docs/languages/go/instrumentation/)
exports batches through a bounded, nonblocking queue. Export attempts time out
after 2 seconds; shutdown allows 5 seconds to flush. A slow collector can lose
traces without blocking task execution. Exporter initialization does not verify
collector connectivity: configuration alone is not evidence of a cloud connection.

Tests use an in-memory span exporter and HTTP recorder, requiring no Docker or
collector. Live trace delivery remains a separate deployment check.
