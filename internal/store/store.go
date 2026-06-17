// Package store implements the durable PostgreSQL queue. Claims and lease
// decisions use database time, independent of worker clock skew.
package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"forge/internal/model"
	"forge/internal/queue"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ pool *pgxpool.Pool }

var _ queue.Store = (*Store)(nil)

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("configure PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// rollback uses a bounded independent context even when the caller timed out.
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// Migrate serializes schema changes across processes. It is called explicitly
// by the deployment migration step, not by every worker at startup.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(706012026)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS forge_schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return err
	}
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, file := range files {
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM forge_schema_migrations WHERE name=$1)`, file.Name()).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + file.Name())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", file.Name(), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO forge_schema_migrations (name) VALUES ($1)`, file.Name()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) Create(ctx context.Context, spec model.Spec, key string) (model.Workflow, bool, error) {
	if err := model.Validate(spec); err != nil {
		return model.Workflow{}, false, err
	}
	if len(key) > 200 {
		return model.Workflow{}, false, errors.New("idempotency key exceeds 200 bytes")
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return model.Workflow{}, false, err
	}
	hash := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(hash[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Workflow{}, false, err
	}
	defer rollback(tx)
	id := model.ID()
	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO forge_workflows (id,name,request_hash,idempotency_key)
		VALUES ($1,$2,$3,NULLIF($4,'')) ON CONFLICT (idempotency_key) DO NOTHING RETURNING created_at`,
		id, spec.Name, fingerprint, key).Scan(&createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var previousHash string
		// A separate READ COMMITTED statement sees the winning insert after
		// ON CONFLICT waits for its transaction to commit.
		if err := tx.QueryRow(ctx, `SELECT id,request_hash FROM forge_workflows WHERE idempotency_key=$1`, key).Scan(&id, &previousHash); err != nil {
			return model.Workflow{}, false, err
		}
		if previousHash != fingerprint {
			return model.Workflow{}, false, model.ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return model.Workflow{}, false, err
		}
		w, err := s.Get(ctx, id)
		return w, false, err
	}
	if err != nil {
		return model.Workflow{}, false, err
	}
	w := model.Workflow{ID: id, Name: spec.Name, CreatedAt: createdAt, State: model.Running, Tasks: make([]model.Task, 0, len(spec.Tasks))}
	rows := make([][]any, 0, len(spec.Tasks))
	for i, task := range spec.Tasks {
		task.DependsOn = append([]string{}, task.DependsOn...)
		data, err := json.Marshal(task)
		if err != nil {
			return model.Workflow{}, false, err
		}
		rows = append(rows, []any{id, task.ID, i, data, task.DependsOn, task.MaxAttempts})
		w.Tasks = append(w.Tasks, model.Task{TaskSpec: task, WorkflowID: id, State: model.Pending})
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"forge_tasks"}, []string{"workflow_id", "id", "ordinal", "spec", "depends_on", "max_attempts"}, pgx.CopyFromRows(rows))
	if err != nil {
		return model.Workflow{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Workflow{}, false, err
	}
	return w, true, nil
}

const taskColumns = `t.spec,t.workflow_id,t.state,t.worker_id,t.claim,t.attempt,t.tokens,t.reason,t.lease_until,t.started_at,t.finished_at`

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (model.Task, error) {
	var t model.Task
	var data []byte
	var state string
	var lease *time.Time
	err := row.Scan(&data, &t.WorkflowID, &state, &t.WorkerID, &t.Claim, &t.Attempt, &t.Tokens, &t.Reason, &lease, &t.StartedAt, &t.FinishedAt)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(data, &t.TaskSpec); err != nil {
		return t, err
	}
	t.State = model.State(state)
	if lease != nil {
		t.LeaseUntil = *lease
	}
	return t, nil
}

func get(ctx context.Context, tx pgx.Tx, id string) (model.Workflow, error) {
	w := model.Workflow{Tasks: []model.Task{}}
	err := tx.QueryRow(ctx, `SELECT id,name,created_at FROM forge_workflows WHERE id=$1`, id).Scan(&w.ID, &w.Name, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, model.ErrNotFound
	}
	if err != nil {
		return w, err
	}
	rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM forge_tasks t WHERE workflow_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return w, err
		}
		w.Tasks = append(w.Tasks, task)
	}
	w.State = model.Aggregate(w.Tasks)
	return w, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (model.Workflow, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.Workflow{}, err
	}
	defer rollback(tx)
	w, err := get(ctx, tx, id)
	if err != nil {
		return w, err
	}
	return w, tx.Commit(ctx)
}

func (s *Store) List(ctx context.Context, limit int) ([]model.Workflow, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("list limit must be 1–100")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT id FROM forge_workflows ORDER BY created_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]model.Workflow, 0, len(ids))
	for _, id := range ids {
		w, err := get(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, tx.Commit(ctx)
}

func (s *Store) Claim(ctx context.Context, worker string, lease time.Duration) (*model.Task, error) {
	if lease.Microseconds() < 1 || worker == "" {
		return nil, errors.New("worker ID and positive lease required")
	}
	t, err := scanTask(s.pool.QueryRow(ctx, `WITH candidate AS (
		SELECT t.workflow_id,t.id FROM forge_tasks t
		WHERE t.state='pending' AND t.attempt < t.max_attempts AND NOT EXISTS (
			SELECT 1 FROM unnest(t.depends_on) AS d(id)
			LEFT JOIN forge_tasks parent ON parent.workflow_id=t.workflow_id AND parent.id=d.id
			WHERE parent.state IS DISTINCT FROM 'succeeded'
		)
		ORDER BY t.workflow_id,t.ordinal FOR UPDATE OF t SKIP LOCKED LIMIT 1
	) UPDATE forge_tasks t SET state='running',worker_id=$1,claim=$2,attempt=t.attempt+1,
		tokens=0,reason='',started_at=clock_timestamp(),finished_at=NULL,
		lease_until=clock_timestamp()+$3::bigint * interval '1 microsecond'
	FROM candidate c WHERE t.workflow_id=c.workflow_id AND t.id=c.id
	RETURNING `+taskColumns, worker, model.ID(), lease.Microseconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) Renew(ctx context.Context, task model.Task, lease time.Duration) (bool, error) {
	if lease.Microseconds() < 1 {
		return false, errors.New("positive lease required")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE forge_tasks SET lease_until=clock_timestamp()+$4::bigint * interval '1 microsecond'
		WHERE workflow_id=$1 AND id=$2 AND claim=$3 AND state='running' AND lease_until>clock_timestamp()`, task.WorkflowID, task.ID, task.Claim, lease.Microseconds())
	return tag.RowsAffected() == 1, err
}

// propagate marks every pending descendant of a terminal failure as skipped.
// UNION de-duplicates a child reachable from multiple failed ancestors.
func propagate(ctx context.Context, tx pgx.Tx, workflowID string) error {
	_, err := tx.Exec(ctx, `WITH RECURSIVE blocked AS (
		SELECT workflow_id,id FROM forge_tasks WHERE state IN ('failed','cancelled','skipped') AND ($1='' OR workflow_id=$1)
		UNION
		SELECT t.workflow_id,t.id FROM forge_tasks t JOIN blocked b
		ON t.workflow_id=b.workflow_id AND b.id=ANY(t.depends_on) WHERE t.state='pending'
	) UPDATE forge_tasks t SET state='skipped',reason='dependency_failed',finished_at=clock_timestamp()
	FROM blocked b WHERE t.workflow_id=b.workflow_id AND t.id=b.id AND t.state='pending'`, workflowID)
	return err
}

func (s *Store) Finish(ctx context.Context, task model.Task, state model.State, tokens int, reason string) (bool, error) {
	if (state != model.Succeeded && state != model.Failed && state != model.Cancelled) || tokens < 0 || tokens > task.TokenLimit {
		return false, errors.New("invalid task completion")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, `UPDATE forge_tasks SET state=$4,tokens=$5,reason=$6,finished_at=clock_timestamp()
		WHERE workflow_id=$1 AND id=$2 AND claim=$3 AND state='running' AND lease_until>clock_timestamp()`, task.WorkflowID, task.ID, task.Claim, string(state), tokens, reason)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if state != model.Succeeded {
		if err := propagate(ctx, tx, task.WorkflowID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) Cancel(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM forge_workflows WHERE id=$1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return model.ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE forge_tasks SET state='cancelled',reason='user_cancelled',finished_at=clock_timestamp()
		WHERE workflow_id=$1 AND state IN ('pending','running')`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Reap(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	_, err = tx.Exec(ctx, `WITH expired AS (
		SELECT workflow_id,id FROM forge_tasks WHERE state='running' AND lease_until<=clock_timestamp()
		ORDER BY lease_until FOR UPDATE SKIP LOCKED LIMIT 1000
	) UPDATE forge_tasks t SET state=CASE WHEN t.attempt>=t.max_attempts THEN 'failed' ELSE 'pending' END,
		reason='lease_expired',claim='',worker_id='',lease_until=NULL,
		finished_at=CASE WHEN t.attempt>=t.max_attempts THEN clock_timestamp() ELSE NULL END
	FROM expired e WHERE t.workflow_id=e.workflow_id AND t.id=e.id`)
	if err != nil {
		return err
	}
	if err := propagate(ctx, tx, ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Heartbeat(ctx context.Context, n model.Node) error {
	if n.Capacity == 0 {
		_, err := s.pool.Exec(ctx, `DELETE FROM forge_nodes WHERE id=$1`, n.ID)
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO forge_nodes (id,capacity,active) VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET capacity=EXCLUDED.capacity,active=EXCLUDED.active,last_seen=clock_timestamp()`, n.ID, n.Capacity, n.Active)
	return err
}

func (s *Store) Nodes(ctx context.Context) ([]model.Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,capacity,active,last_seen FROM forge_nodes WHERE last_seen>clock_timestamp()-interval '10 seconds' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := []model.Node{}
	for rows.Next() {
		var n model.Node
		if err := rows.Scan(&n.ID, &n.Capacity, &n.Active, &n.LastSeen); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
