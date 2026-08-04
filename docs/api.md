# Forge HTTP API

The API accepts declarative workflows for simulated agents. No model provider,
cloud account, or executable code is invoked. Run `go run ./cmd/forge -mode demo`
to start a local API with embedded workers; see the repository README for setup.

All `/v1` endpoints require `Authorization: Bearer <token>` when
`FORGE_API_TOKEN` is configured. An empty token is intended for a loopback demo.
Health probes remain public and expose no workflow data. There is no CORS grant;
the operations console uses same-origin requests. The public root path serves
only the static console shell; API data still requires authentication.

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/healthz` | Process is responsive; 200 |
| GET | `/readyz` | Queue responds; 200, or 503 when unavailable |
| GET | `/metrics` | Prometheus metrics; same bearer authentication as `/v1` |
| POST | `/v1/workflows` | Validate and submit a workflow; 201 |
| GET | `/v1/workflows?limit=20` | Latest 1–100 workflow summaries with task counts |
| GET | `/v1/workflows/{id}` | Workflow and all tasks |
| POST | `/v1/workflows/{id}/cancel` | Cancel pending/running tasks; 204 |
| GET | `/v1/nodes` | Workers with a heartbeat within the last 10 seconds |
| GET | `/v1/system` | Mode, storage, region profile, and trace configuration |

System metadata always reports the cloud profile as simulated and disconnected.
It exposes neither credentials nor database/collector URLs. A configured trace
exporter is not a health check for collector delivery.

## Submission

Use `Content-Type: application/json`. Unknown fields, malformed JSON, multiple
JSON values, invalid dependency graphs, and bodies larger than 4 MiB are rejected.
Every task must specify execution budgets; see `examples/workflow.json`.

```sh
curl -i http://127.0.0.1:8080/v1/workflows \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: research-demo-1' \
  --data-binary @examples/workflow.json
```

Add `-H "Authorization: Bearer $FORGE_API_TOKEN"` when authentication is enabled.
The response contains the workflow ID, and `Location` points to its detail URL.
`Idempotency-Key` is optional and limited to 200 bytes. Repeating the same key and
parsed workflow returns the existing workflow with 200. Reusing the key with
different content returns 409. Keys last as long as their workflow data;
in-memory demo keys disappear on restart.

Execution is asynchronous. Poll the detail endpoint to inspect task states and
breaker reasons. The list endpoint returns summaries rather than task payloads.
It is a bounded recent-history view, not a paginated full-history export.

Cancellation is idempotent for existing workflows. It preserves already terminal
tasks. A running executor notices cancellation on its next lease renewal; worker
defaults renew every 500 ms, with a 1-second store timeout. Database outages or
scheduler stalls can affect those timings. A worker cannot overwrite a cancelled
task with a late completion.

## Errors and deadlines

API handler errors use this shape:

```json
{
  "error": {"code": "invalid_workflow", "message": "dependency cycle detected"},
  "request_id": "a server-generated request identifier"
}
```

Responses include `X-Request-ID`, `Cache-Control: no-store`, and
`X-Content-Type-Options: nosniff`. Lease claim tokens are never serialized.
Invalid input returns 400; invalid credentials 401; unknown workflows 404;
idempotency conflicts 409; excessive body size 413; wrong content type 415;
queue outages 503; and request deadline expiry 504. Chi returns 405 for an
unsupported method. Internal database errors are logged, not returned to clients.

Requests have a default 5-second context deadline. The store honors this deadline
for I/O; it is not a mechanism for forcibly terminating arbitrary Go code.
Read/header/write timeouts are configured on the application HTTP server.
