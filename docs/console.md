# Operations console

The console is embedded in the Forge binary and served at the API's root path.
It needs no frontend build step, CDN, external font, or browser framework.
See [Getting started](getting-started.md) for startup commands.

## Workflow controls

Choose **New workflow** to submit one of four presets:

| Preset | Behavior |
| --- | --- |
| Parallel batch | Independent tasks, approximately 5 seconds each |
| Research pipeline | Three tasks with sequential dependencies |
| Token breaker | Runaway agents stopped by a 100-token budget |
| Runtime breaker | Runaway agents stopped by a 1.5-second budget |

Batch and breaker presets accept 1–2,200 tasks. The pipeline has three tasks.
The console retains the submission's idempotency key on an error, so retrying the
same form does not create a duplicate workflow when the original response was
lost. Editing the specification creates a new key. Reloading the page resets it.

Search workflows by name or ID and filter by state. **Inspect** opens task-level
states, token usage, attempts, worker assignment, and failure reasons. The task
inspector pages through 100 tasks at a time. **Cancel workflow** terminates
pending/running work without changing already terminal tasks.

Filter **Worker pools** by **Busy** to find pools executing tasks or **Available
capacity** to find pools with free task slots. A busy pool can still have capacity.
The filter stays selected as heartbeats refresh; overview totals cover the full fleet.

## Interpreting the dashboard

The workflow table and task cards cover the latest 100 workflows. They are not
all-time account totals. The worker activity chart records heartbeat samples
collected during this browser session, up to 60 points. It is not a historical
metrics database, and heartbeat-based activity can lag short tasks.

The console polls every two seconds, avoids overlapping refreshes, and pauses
polling in hidden tabs. A failed refresh displays a stale-data notice instead of
inventing activity. Worker cards include only recent heartbeats. In demo mode,
the pools share one process; in API mode, they represent independent workers.

## Execution plane and access

The execution plane card shows configured region, task capacity, and active
attempts from API metadata and worker heartbeats. Its status indicates whether
workers are reporting; a failed refresh marks the card and worker status stale.
The region is a display setting from `FORGE_CLOUD_REGION`, defaulting to
`us-east-1`, and does not verify cloud hosting. The console uses the same
operations view across execution modes; `/v1/system` retains runtime metadata.

**Execution policies** explains dependencies, budgets, leases, and cancellation,
and links to the [cloud deployment guide](cloud-deployment.md). Deployment
commands and storage implementation details belong in the operator guides.

If API authentication is enabled, use **Connection settings** to enter the bearer
token. It is held in page memory, never in local storage or cookies, and cleared
on reload. The static console shell is public; workflow data and system metadata
remain behind API authentication. Serve remote deployments over HTTPS.

The console uses same-origin requests, renders API values as text, and serves
an explicit allowlist of embedded assets with a Content Security Policy. Worker
metrics listeners do not expose the console or workflow API.

## Checks

```sh
node --test internal/console/model_test.mjs
go test -race ./internal/console ./internal/api ./internal/app
```

These tests cover preset budgets, filtering, aggregation, assets, access controls,
and application routing. Browser interaction and visual verification are separate
checks; they require running the application in a browser-capable environment.
