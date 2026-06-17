CREATE TABLE forge_workflows (
    id text PRIMARY KEY,
    name text NOT NULL,
    request_hash text NOT NULL,
    idempotency_key text UNIQUE,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE forge_tasks (
    workflow_id text NOT NULL REFERENCES forge_workflows(id) ON DELETE CASCADE,
    id text NOT NULL,
    ordinal integer NOT NULL,
    spec jsonb NOT NULL,
    depends_on text[] NOT NULL DEFAULT '{}',
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled', 'skipped')),
    worker_id text NOT NULL DEFAULT '',
    claim text NOT NULL DEFAULT '',
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    max_attempts integer NOT NULL CHECK (max_attempts BETWEEN 1 AND 5),
    tokens integer NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    reason text NOT NULL DEFAULT '',
    lease_until timestamptz,
    started_at timestamptz,
    finished_at timestamptz,
    PRIMARY KEY (workflow_id, id),
    UNIQUE (workflow_id, ordinal),
    CHECK (state <> 'running' OR (lease_until IS NOT NULL AND claim <> ''))
);

CREATE INDEX forge_workflows_created ON forge_workflows (created_at DESC, id);
CREATE INDEX forge_tasks_pending ON forge_tasks (workflow_id, ordinal) WHERE state = 'pending';
CREATE INDEX forge_tasks_expired ON forge_tasks (lease_until) WHERE state = 'running';

CREATE TABLE forge_nodes (
    id text PRIMARY KEY,
    capacity integer NOT NULL CHECK (capacity > 0),
    active integer NOT NULL CHECK (active BETWEEN 0 AND capacity),
    last_seen timestamptz NOT NULL DEFAULT clock_timestamp()
);
