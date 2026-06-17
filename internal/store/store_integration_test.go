//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"forge/internal/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Each test owns a fresh schema, so it never truncates another test's data.
func database(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is required; see docs/testing.md")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "forge_test_" + model.ID()
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{pool: pool}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	return s, schema
}

func task(id string) model.TaskSpec {
	return model.TaskSpec{ID: id, Steps: 2, StepMS: 10, TokensPerStep: 5, TokenLimit: 100, RuntimeMS: 1000, MaxAttempts: 2}
}

func create(t *testing.T, s *Store, tasks ...model.TaskSpec) model.Workflow {
	t.Helper()
	w, fresh, err := s.Create(context.Background(), model.Spec{Name: "integration", Tasks: tasks}, "")
	if err != nil || !fresh {
		t.Fatalf("create: fresh=%v err=%v", fresh, err)
	}
	return w
}

func claim(t *testing.T, s *Store) model.Task {
	t.Helper()
	task, err := s.Claim(context.Background(), "worker", time.Minute)
	if err != nil || task == nil {
		t.Fatalf("claim: task=%+v err=%v", task, err)
	}
	return *task
}

func finish(t *testing.T, s *Store, task model.Task, state model.State) {
	t.Helper()
	ok, err := s.Finish(context.Background(), task, state, 10, "test")
	if err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}
}

func TestPersistenceAndIdempotency(t *testing.T) {
	s, schema := database(t)
	ctx := context.Background()
	spec := model.Spec{Name: "persistent", Tasks: []model.TaskSpec{task("one")}}
	first, fresh, err := s.Create(ctx, spec, "request-1")
	if err != nil || !fresh {
		t.Fatalf("create: %v", err)
	}
	second, fresh, err := s.Create(ctx, spec, "request-1")
	if err != nil || fresh || first.ID != second.ID {
		t.Fatalf("duplicate request: %+v %v", second, err)
	}
	spec.Name = "different"
	if _, _, err := s.Create(ctx, spec, "request-1"); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	finish(t, s, claim(t, s), model.Succeeded)
	s.Close()
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reopened := &Store{pool: pool}
	t.Cleanup(reopened.Close)
	got, err := reopened.Get(ctx, first.ID)
	if err != nil || got.State != model.Succeeded || got.Tasks[0].Tokens != 10 {
		t.Fatalf("lost persisted data: %+v %v", got, err)
	}
	listed, err := reopened.List(ctx, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatalf("list: %+v %v", listed, err)
	}
	if _, err := reopened.Get(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	if err := reopened.Cancel(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing cancel: %v", err)
	}
}

func TestConcurrentIdempotentCreation(t *testing.T) {
	s, _ := database(t)
	ctx := context.Background()
	const count = 12
	type result struct {
		id    string
		fresh bool
		err   error
	}
	results := make(chan result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, fresh, err := s.Create(ctx, model.Spec{Name: "same", Tasks: []model.TaskSpec{task("one")}}, "shared-key")
			results <- result{w.ID, fresh, err}
		}()
	}
	wg.Wait()
	close(results)
	id, inserts := "", 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if id != "" && result.id != id {
			t.Fatal("idempotency race created multiple workflows")
		}
		id = result.id
		if result.fresh {
			inserts++
		}
	}
	if inserts != 1 {
		t.Fatalf("expected one insert, got %d", inserts)
	}
}

func TestParallelClaimsAreExclusive(t *testing.T) {
	s, _ := database(t)
	ctx := context.Background()
	const count = 80
	tasks := make([]model.TaskSpec, count)
	for i := range tasks {
		tasks[i] = task(fmt.Sprintf("task-%03d", i))
	}
	created := create(t, s, tasks...)
	claimed := make(chan model.Task, count)
	errors := make(chan error, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				task, err := s.Claim(ctx, fmt.Sprintf("node-%d", worker), time.Minute)
				if err != nil {
					errors <- err
					return
				}
				if task == nil {
					return
				}
				claimed <- *task
			}
		}(i)
	}
	wg.Wait()
	close(claimed)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for task := range claimed {
		if seen[task.ID] || task.Attempt != 1 || task.Claim == "" {
			t.Fatalf("duplicate/invalid claim: %+v", task)
		}
		seen[task.ID] = true
		finish(t, s, task, model.Succeeded)
	}
	if len(seen) != count {
		t.Fatalf("claimed %d of %d", len(seen), count)
	}
	w, err := s.Get(ctx, created.ID)
	if err != nil || w.State != model.Succeeded {
		t.Fatalf("workflow: %+v %v", w, err)
	}
}

func TestDependencySchedulingAndFailurePropagation(t *testing.T) {
	s, _ := database(t)
	ctx := context.Background()
	root, child, grandchild := task("root"), task("child"), task("grandchild")
	child.DependsOn = []string{"root"}
	grandchild.DependsOn = []string{"child"}
	created := create(t, s, root, child, grandchild)
	first := claim(t, s)
	if first.ID != "root" {
		t.Fatalf("claimed dependency early: %s", first.ID)
	}
	if next, err := s.Claim(ctx, "other", time.Minute); err != nil || next != nil {
		t.Fatalf("dependent runnable early: %+v %v", next, err)
	}
	finish(t, s, first, model.Succeeded)
	second := claim(t, s)
	if second.ID != "child" {
		t.Fatalf("dependency order: %s", second.ID)
	}
	finish(t, s, second, model.Failed)
	w, err := s.Get(ctx, created.ID)
	if err != nil || w.State != model.Failed || w.Tasks[2].State != model.Skipped || w.Tasks[2].Attempt != 0 {
		t.Fatalf("failure propagation: %+v %v", w, err)
	}
}

func expire(t *testing.T, s *Store, task model.Task) {
	t.Helper()
	// Move database time for this lease, without fragile sleeps or clock mocks.
	_, err := s.pool.Exec(context.Background(), `UPDATE forge_tasks SET lease_until=clock_timestamp()-interval '1 second' WHERE workflow_id=$1 AND id=$2`, task.WorkflowID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLeaseRecoveryFencesOldWorkersAndBoundsAttempts(t *testing.T) {
	s, _ := database(t)
	ctx := context.Background()
	root, child := task("root"), task("child")
	child.DependsOn = []string{"root"}
	created := create(t, s, root, child)
	old := claim(t, s)
	// Renewal ignores worker clock fields and consults the database lease.
	skewed := old
	skewed.LeaseUntil = time.Time{}
	if ok, err := s.Renew(ctx, skewed, time.Minute); err != nil || !ok {
		t.Fatalf("clock-skew renewal: %v %v", ok, err)
	}
	expire(t, s, old)
	if ok, err := s.Renew(ctx, old, time.Minute); err != nil || ok {
		t.Fatalf("expired lease renewed: %v %v", ok, err)
	}
	if ok, err := s.Finish(ctx, old, model.Succeeded, 10, "stale"); err != nil || ok {
		t.Fatalf("expired completion accepted: %v %v", ok, err)
	}
	if err := s.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	replacement := claim(t, s)
	if replacement.Attempt != 2 || replacement.Claim == old.Claim {
		t.Fatalf("invalid replacement: %+v", replacement)
	}
	if ok, err := s.Renew(ctx, old, time.Minute); err != nil || ok {
		t.Fatalf("old worker renewed replacement: %v %v", ok, err)
	}
	if ok, err := s.Finish(ctx, old, model.Succeeded, 10, "stale"); err != nil || ok {
		t.Fatalf("old worker overwrote replacement: %v %v", ok, err)
	}
	expire(t, s, replacement)
	if err := s.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := s.Get(ctx, created.ID)
	if err != nil || w.Tasks[0].State != model.Failed || w.Tasks[0].Reason != "lease_expired" || w.Tasks[1].State != model.Skipped {
		t.Fatalf("exhausted retries: %+v %v", w, err)
	}
	if next, err := s.Claim(ctx, "third", time.Minute); err != nil || next != nil {
		t.Fatalf("retry bound exceeded: %+v %v", next, err)
	}
}

func TestCancellationPreventsFurtherClaimsAndCompletion(t *testing.T) {
	s, _ := database(t)
	ctx := context.Background()
	created := create(t, s, task("running"), task("pending"))
	running := claim(t, s)
	if err := s.Cancel(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Finish(ctx, running, model.Succeeded, 10, "late"); err != nil || ok {
		t.Fatalf("cancel overwritten: %v %v", ok, err)
	}
	if ok, err := s.Renew(ctx, running, time.Minute); err != nil || ok {
		t.Fatalf("cancelled claim renewed: %v %v", ok, err)
	}
	if next, err := s.Claim(ctx, "other", time.Minute); err != nil || next != nil {
		t.Fatalf("cancelled work claimed: %+v %v", next, err)
	}
	w, err := s.Get(ctx, created.ID)
	if err != nil || w.State != model.Cancelled {
		t.Fatalf("cancellation: %+v %v", w, err)
	}
}

func TestNodeHeartbeatExpiryAndRetirement(t *testing.T) {
	s, _ := database(t)
	ctx := context.Background()
	if err := s.Heartbeat(ctx, model.Node{ID: "one", Capacity: 256, Active: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, model.Node{ID: "one", Capacity: 256, Active: 20}); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.Nodes(ctx)
	if err != nil || len(nodes) != 1 || nodes[0].Active != 20 {
		t.Fatalf("heartbeat: %+v %v", nodes, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE forge_nodes SET last_seen=clock_timestamp()-interval '11 seconds'`); err != nil {
		t.Fatal(err)
	}
	nodes, err = s.Nodes(ctx)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("expired node still live: %+v %v", nodes, err)
	}
	if err := s.Heartbeat(ctx, model.Node{ID: "one", Capacity: 256}); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, model.Node{ID: "one", Capacity: 0}); err != nil {
		t.Fatal(err)
	}
	nodes, err = s.Nodes(ctx)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("retired node still live: %+v %v", nodes, err)
	}
}
