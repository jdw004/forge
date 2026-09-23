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

With Node.js 22 or newer and Python 3 installed, `make check` also runs the
console model and configuration-script tests and builds both executables. It
does not start containers or contact cloud services. `make compose-check` parses
the deployment configuration using the Docker CLI without starting a daemon.

## Continuous integration

The [CI workflow](../.github/workflows/ci.yml) runs on pushes to `main`, pull
requests, and manual dispatch. Default jobs check Go 1.24 and the current stable
toolchain, race detection, vet, formatting, module integrity, executable builds,
console logic, credential-file generation, Compose configuration, and
CloudFormation resource schemas with
[cfn-lint](https://github.com/aws-cloudformation/cfn-lint). The integration suite
is compiled in default CI but is not executed without a database.

The optional **containers** input on a manual workflow run enables PostgreSQL
integration tests, a Docker image build, and an image startup smoke test. It is
disabled by default, so pushes and pull requests start no containers. The
database runs as a disposable
[GitHub Actions service](https://docs.github.com/en/actions/tutorials/use-containerized-services/create-postgresql-service-containers)
with CI-only credentials. No AWS credentials, deployments, or image publishing
are configured in the workflow.

The workflow must run on GitHub before its results can be reported as passing.
Local checks do not establish PostgreSQL runtime behavior, a functioning cloud
deployment, or browser rendering. The optional image smoke test checks binary
startup only; full-stack HTTP, collector delivery, and cloud routing still need
their own deployment checks.

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
