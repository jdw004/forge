# Load testing

`forge-load` submits reproducible simulator workloads and writes a JSON report.
It exits nonzero on invalid configuration, timeout, transport errors, or unexpected
task outcomes. The `batch`, `token`, and `runtime` scenarios expect successful
completion, token-budget termination, and runtime-budget termination respectively.
Every task must finish in exactly one attempt for the report to pass.

## Embedded engine

Run from the repository root with Go installed:

```sh
go run ./cmd/forge-load -tasks 2200 > batch.json
go run ./cmd/forge-load -scenario token -tasks 100 -workers 10 -concurrency 16 > token.json
go run ./cmd/forge-load -scenario runtime -tasks 100 -workers 10 -concurrency 16 > runtime.json
```

The default batch uses ten worker pools, 256 slots per pool, and 2,200 independent
tasks with five seconds of simulated work each. It uses the actual worker and
executor implementations with an ephemeral memory store. No server, database,
container, or network connection is required. Worker registration happens before
the measurement starts. Pools stop before the command returns, including on
interruption or deadline expiry.

`-task-duration`, `-runtime-limit`, `-timeout`, and `-poll` accept Go duration
strings, such as `5s` or `100ms`. Task counts are limited to 5,000; embedded
capacity is limited to 4,096 slots across at most ten pools. `make load` runs the
default batch and `make build` produces `bin/forge-load` alongside `bin/forge`.

## Existing deployment

HTTP mode submits work to an existing API and observes its registered workers:

```sh
export FORGE_API_TOKEN_FILE="$PWD/.secrets/api_token"
go run ./cmd/forge-load -backend http -url https://YOUR_FORGE_ENDPOINT \
  -tasks 2200 -timeout 2m > deployment-batch.json
```

Replace the endpoint with your deployment URL. The token must match the API;
`FORGE_API_TOKEN` is also supported, but cannot be combined with its `_FILE` form.
HTTP mode does not provision workers. The `-workers` and `-concurrency` flags
configure embedded runs only. Remote capacity comes from `/v1/nodes` and the
execution mode comes from `/v1/system`. Redirects are rejected.

When a submitted workflow is known, an interrupted or failed run attempts to
cancel that workflow with a separate five-second timeout. Inspect `cleanup` in
the report. A lost or unreadable submission response can leave an unknown
workflow running; `submission_outcome_unknown` records this case. The report's
`submission_key` is the idempotency key sent with the request. Inspect the
workflow list for the matching load workload before resubmitting. No automatic
submission retries are performed.

## Reading the report

- `configured_capacity` is available capacity, not a concurrency measurement.
- Embedded `peak_active_attempts` counts observer lifetimes from execution start
  through result acknowledgement. HTTP mode samples running task states, so its
  peak is a lower bound and can miss short tasks between polls.
- Embedded latency measures executor elapsed time. HTTP latency uses persisted
  start and finish timestamps, including acknowledgement. Neither includes time
  waiting for a worker. Remote measurements require synchronized host clocks.
- `elapsed_ms` measures submission through completion detection, including poll
  delay and, for embedded runs, worker shutdown. The terminal-result rate is
  workload-specific throughput over that interval.
- `workers_observed` counts distinct worker IDs in task results. It can be lower
  than the registered pool count when a short batch finishes on fewer workers.
- `passed` checks task outcomes and attempt counts. It does not enforce a latency
  threshold or prove a particular concurrency level; inspect those measurements
  separately. Partial reports reflect the last sample before cleanup.

## Recorded results

These are individual measured runs on macOS arm64 with Go 1.27.3, using the
single-process memory backend and deterministic simulator. Raw reports retain
the actual measurement timestamps and configuration.

| Scenario | Tasks | Pools / slots | Peak active attempts | Wall time | Maximum executor latency | Outcome |
| --- | ---: | --- | ---: | ---: | ---: | --- |
| [Batch](../benchmarks/memory-batch.json) | 2,200 | 10 / 2,560 | 2,200 | 6.707 s | 5.009 s | 2,200 succeeded |
| [Token breaker](../benchmarks/memory-token.json) | 100 | 10 / 160 | 100 | 0.301 s | 0.131 s | 100 token-limit failures |
| [Runtime breaker](../benchmarks/memory-runtime.json) | 100 | 10 / 160 | 100 | 1.201 s | 1.002 s | 100 runtime-limit failures |

The token scenario uses 10 tokens per 25 ms step with a 50-token budget. The
runtime scenario uses a one-second budget and a sufficiently large token budget
to isolate the runtime breaker. Both deliberately request runaway execution.

These measurements demonstrate embedded worker concurrency and cooperative
budget enforcement. They do not establish performance across ten separate
machines, PostgreSQL throughput, or a percentage reduction in infinite loops.
The latter requires a defined baseline and comparative experiment. Container
and PostgreSQL runtime validation remain separate from these recorded runs.
