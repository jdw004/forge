# Container deployment

The Docker image contains the Forge binary, embedded console, and migrations.
It runs as UID 10001 with CA certificates for HTTPS and PostgreSQL TLS. Compose
runs the application with a read-only root filesystem, dropped capabilities,
and a 20-second shutdown grace period.

## Generate credentials

```sh
python3 scripts/configure-compose.py
```

This creates a private `.secrets` directory with independent random database and
API credentials. It refuses an existing directory, never prints credentials,
and starts no containers. `.secrets` is excluded from Git and the Docker build
context. The files are readable by the non-root container UID; the containing
directory is accessible only to the host user.

Forge supports `FORGE_API_TOKEN_FILE` and `DATABASE_URL_FILE` for mounted secrets.
Do not set both the plain variable and its `_FILE` variant. File values are
bounded to 8 KiB, and trailing newlines are removed. Compose mounts secrets
read-only; see [Docker's secret documentation](https://docs.docker.com/compose/how-tos/use-secrets/).

## Start the execution stack

When ready to run Docker:

```sh
docker compose up --build -d --scale worker=10
docker compose ps
```

Compose starts PostgreSQL, waits for its health check, runs migrations, then
starts the API and workers. A failed migration prevents application startup.
The [dependency conditions](https://docs.docker.com/compose/how-tos/startup-order/)
are startup gates, not a mechanism for restarting services after a runtime failure.

The API and console are at `http://127.0.0.1:8080`. Read your generated token and
enter it under **Connection settings**:

```sh
cat .secrets/api_token
```

PostgreSQL and worker listeners have no published host ports. The API is published
only on loopback. `FORGE_API_PORT` changes that host port. Each worker uses its
own generated identity and has a metrics endpoint on container port 9091.

There are 10 independent worker processes with 256 slots each by default. This
is configured capacity, not a measured concurrency result. The generated database
URL caps each process's pool at eight connections: 10 workers plus one API can
use up to 88 connections. Increasing replicas requires budgeting database
connections as well as CPU and memory.

Use fewer workers or slots for a smaller machine:

```sh
FORGE_CONCURRENCY=32 docker compose up --build -d --scale worker=2
```

## Optional monitoring

```sh
docker compose -f compose.yaml -f compose.observability.yaml up --build -d --scale worker=10
```

The additional configuration starts Prometheus and an OpenTelemetry Collector.
Prometheus is at `http://127.0.0.1:9090` and retains seven days of metrics in a
named volume. Its worker job uses
[DNS discovery](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#dns_sd_config)
to scrape each worker replica separately with the API bearer token.

The collector receives traces on an internal OTLP/HTTP endpoint and uses a
[debug exporter](https://opentelemetry.io/docs/collector/troubleshooting/)
to write sampled spans to its container logs:

```sh
docker compose -f compose.yaml -f compose.observability.yaml logs --tail=100 otel-collector
```

This is a development trace sink, not a durable trace database or cloud service.
The default root sample ratio is 0.1; set `FORGE_TRACE_SAMPLE_RATIO=1` when you
want every root trace for a small test. Configure an exporter for your trace
backend when deploying remotely. The collector's receiver is not published to
the host. `FORGE_PROMETHEUS_PORT` changes the published Prometheus port.

## Stop and preserve data

```sh
docker compose down
# If monitoring was enabled:
docker compose -f compose.yaml -f compose.observability.yaml down
```

These commands retain named database and metrics volumes. The secret files must
be retained with that database. Changing the PostgreSQL password file does not
change the password in an already initialized database; coordinate database-side
credential rotation and the connection URL instead of regenerating secrets.

## Configuration checks

```sh
docker compose config --quiet
docker compose -f compose.yaml -f compose.observability.yaml config --quiet
python3 -m unittest discover -s scripts -p 'test_*.py'
```

Configuration validation requires no running daemon and starts no services.
Container image builds, image pulls, database integration, DNS target discovery,
and live collector delivery still require a Docker runtime check. They have not
been established by configuration validation alone.
