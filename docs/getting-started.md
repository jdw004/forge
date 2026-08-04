# Getting started with Forge

An agent execution engine in Go, with a Chi HTTP API, dependency-aware workflows,
bounded worker pools, and PostgreSQL task leases. Token and runtime circuit
breakers stop the built-in simulated agents. No cloud credentials or model
provider are needed for the local demo.

## Run locally

Install Go 1.24 or newer, then:

```sh
go mod download
go run ./cmd/forge -mode demo
```

Forge listens at `127.0.0.1:8080` and starts 10 embedded worker pools with 256
task slots each. These are pools in one process, not 10 separate machines.
The store starts empty and loses its data when the process exits.

Open **http://127.0.0.1:8080** for the operations console. Use **New workflow**
to launch a parallel batch, dependency pipeline, or intentional breaker test.
The [console guide](console.md) explains its controls and data scope.

In another terminal:

```sh
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/v1/nodes
curl -i http://127.0.0.1:8080/v1/workflows \
  -H 'Content-Type: application/json' \
  --data-binary @examples/workflow.json
curl http://127.0.0.1:8080/v1/workflows
```

Use the returned workflow ID with `/v1/workflows/{id}` to inspect task states.
The [API reference](api.md) covers authentication, idempotency, and cancellation.
Stop the process with Ctrl-C; it closes the listener and joins its workers.

For a smaller demo:

```sh
go run ./cmd/forge -mode demo -demo-nodes 2 -concurrency 8
```

## Modes and configuration

| Mode | Store | Services |
| --- | --- | --- |
| `demo` | In-memory | HTTP API and embedded worker pools |
| `api` | PostgreSQL | HTTP API only |
| `worker` | PostgreSQL | One worker pool; optional metrics listener |
| `migrate` | PostgreSQL | Apply embedded migrations, then exit |

The default mode is `demo`. Flags override environment values. `forge -h` lists
flags. The application reads environment variables directly; it does not load
`.env` automatically. `.env.example` lists development settings.

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `FORGE_MODE` | `demo` | Process mode |
| `FORGE_ADDR` | `127.0.0.1:8080` | API listen address |
| `FORGE_CONCURRENCY` | `256` | Task slots per worker |
| `FORGE_DEMO_NODES` | `10` | Demo pools, between 1 and 10 |
| `FORGE_WORKER_ID` | Generated | Unique identity for an independent worker |
| `FORGE_API_TOKEN` | Empty | Bearer token; use a random secret |
| `DATABASE_URL` | Empty | Required in PostgreSQL modes |
| `FORGE_METRICS_ADDR` | Empty | Optional worker metrics address |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Empty | Optional trace collector base URL |
| `FORGE_TRACE_SAMPLE_RATIO` | `0.1` | Sampling ratio for new root traces |
| `FORGE_CLOUD_REGION` | `us-east-1` | Display region for the simulated cloud profile |

Binding beyond loopback requires an API token of at least 32 bytes. Put a TLS
reverse proxy in front of any remote deployment; the application listener is HTTP.
Never share worker IDs between live independent workers.

## PostgreSQL setup

With an existing PostgreSQL database, set your connection URL and run migrations
once before starting the API or workers:

```sh
export DATABASE_URL='postgres://forge:password@localhost:5432/forge?sslmode=disable'
go run ./cmd/forge -mode migrate
go run ./cmd/forge -mode api
```

Start each independent worker in its own terminal or container with the same
`DATABASE_URL`:

```sh
go run ./cmd/forge -mode worker -concurrency 256
```

Use `sslmode=verify-full` and your provider's CA configuration for remote
PostgreSQL. Migration mode needs schema privileges; normal processes need access
to the queue tables. Migrations serialize with a transaction-level advisory lock.
Worker crashes leave leases that another worker can recover, up to each task's
attempt limit. Execution is at least once across lost leases; future external
effects must be idempotent. Circuit-breaker failures are terminal.

## Validation and scope

Prometheus metrics are available at `/metrics` in API/demo modes. OpenTelemetry
tracing is disabled until a collector endpoint is configured. The
[observability guide](observability.md) covers metrics, tracing setup, and
scraping independent workers.

```sh
go test -race ./...
go vet ./...
go build -o bin/forge ./cmd/forge
```

The [testing guide](testing.md) documents optional PostgreSQL integration
tests. Docker execution is not required for the local demo or unit tests.

The project does not currently make real cloud or LLM calls. Runtime cancellation
is cooperative within the built-in executor; running arbitrary untrusted code
would require process isolation. A 2,560-slot default configuration is capacity,
not evidence of 2,200 concurrent tasks. Throughput and a 75% reduction in loops
require recorded benchmarks and a baseline before they can be claimed as results.
