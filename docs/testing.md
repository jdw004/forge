# Testing Forge

The executor and worker tests use the in-memory store. PostgreSQL integration
tests exercise actual SQL, concurrent clients, persistence across connections,
dependency scheduling, cancellation, lease fencing, and heartbeat expiry.

## Dependencies

Install Go 1.24 or newer. Docker is needed only for the container-based
PostgreSQL integration setup. Download dependencies from a terminal with
network access:

```sh
go mod download
```

Run unit tests and static analysis:

```sh
go test -race ./...
go vet ./...
```

## PostgreSQL integration suite

Start Docker Desktop, then run from the repository root:

```sh
docker compose -f compose.test.yaml up -d --wait
TEST_DATABASE_URL='postgres://forge:forge-test@localhost:5433/forge_test?sslmode=disable' \
  go test -race -tags integration -count=1 -timeout 2m ./internal/store
docker compose -f compose.test.yaml down
```

The database is disposable and local to your machine. It stores its data in
memory and binds only to loopback. If port 5433 is occupied, set `FORGE_TEST_PORT`
when invoking Compose and use that port in `TEST_DATABASE_URL`.

Every test creates a uniquely named schema and drops that schema on cleanup;
the suite does not truncate existing tables. Use a dedicated test database with
permission to create schemas. Integration tests fail if `TEST_DATABASE_URL` is
missing instead of reporting a misleading skipped pass.

The claim query uses PostgreSQL's
[row locks and SKIP LOCKED](https://www.postgresql.org/docs/17/sql-select.html)
to let independent workers claim different tasks. The
[pgx connection pool](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool)
provides bounded database connections and context-aware operations.

The load runner tests exercise all three workload scenarios, timeout cleanup,
authentication, and redirect rejection. See [load testing](load-testing.md) for
commands, recorded engine measurements, and the limits of those measurements.
