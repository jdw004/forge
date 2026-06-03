package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/engine"
	"forge/internal/memory"
	"forge/internal/model"
	"forge/internal/queue"
)

func config(id string, concurrency int) Config {
	return Config{
		ID: id, Concurrency: concurrency, LeaseDuration: 300 * time.Millisecond,
		RenewInterval: 30 * time.Millisecond, StoreTimeout: 50 * time.Millisecond,
		PollInterval: 5 * time.Millisecond, HeartbeatInterval: 30 * time.Millisecond,
		ReapInterval: 20 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func start(t *testing.T, store queue.Store, cfg Config) (*Worker, func()) {
	t.Helper()
	w, err := New(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("worker Run: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("worker did not shut down")
			}
		})
	}
	t.Cleanup(stop)
	return w, stop
}

func eventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func spec(id string) model.TaskSpec {
	return model.TaskSpec{ID: id, Steps: 2, StepMS: 5, TokensPerStep: 3, TokenLimit: 30, RuntimeMS: 3000, MaxAttempts: 3}
}

func create(t *testing.T, store queue.Store, tasks ...model.TaskSpec) model.Workflow {
	t.Helper()
	w, _, err := store.Create(context.Background(), model.Spec{Name: "worker test", Tasks: tasks}, "")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func workflow(t *testing.T, store queue.Store, id string) model.Workflow {
	t.Helper()
	w, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWorkersCoordinateDAGWithoutDuplicateExecution(t *testing.T) {
	store := memory.New()
	tasks := []model.TaskSpec{spec("root")}
	for i := 0; i < 30; i++ {
		task := spec(fmt.Sprintf("child-%d", i))
		task.DependsOn = []string{"root"}
		tasks = append(tasks, task)
	}
	created := create(t, store, tasks...)
	var mu sync.Mutex
	completions := map[string]int{}
	for i := 0; i < 3; i++ {
		cfg := config(fmt.Sprintf("worker-%d", i), 2)
		cfg.OnResult = func(task model.Task, result engine.Result) {
			mu.Lock()
			defer mu.Unlock()
			completions[task.ID]++
		}
		start(t, store, cfg)
	}
	eventually(t, "all DAG tasks completing", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(completions) == len(tasks)
	})
	got := workflow(t, store, created.ID)
	if got.State != model.Succeeded {
		t.Fatalf("workflow state: %s", got.State)
	}
	workers := map[string]bool{}
	for _, task := range got.Tasks {
		if task.Attempt != 1 || task.Tokens != 6 {
			t.Fatalf("duplicate or incomplete task: %+v", task)
		}
		workers[task.WorkerID] = true
		if task.ID != "root" && task.StartedAt.Before(*got.Tasks[0].FinishedAt) {
			t.Fatalf("task %s ran before its dependency completed", task.ID)
		}
	}
	if len(workers) < 2 {
		t.Fatal("work was not distributed across workers")
	}
	mu.Lock()
	defer mu.Unlock()
	for id, count := range completions {
		if count != 1 {
			t.Fatalf("task %s completed %d times", id, count)
		}
	}
}

func TestConcurrencyHeartbeatAndGracefulShutdown(t *testing.T) {
	store := memory.New()
	var tasks []model.TaskSpec
	for i := 0; i < 12; i++ {
		task := spec(fmt.Sprintf("task-%d", i))
		task.Runaway = true
		task.StepMS = 1000
		tasks = append(tasks, task)
	}
	created := create(t, store, tasks...)
	w, stop := start(t, store, config("bounded", 3))
	eventually(t, "heartbeat reporting full capacity", func() bool {
		nodes, err := store.Nodes(context.Background())
		return err == nil && len(nodes) == 1 && nodes[0].Capacity == 3 && nodes[0].Active == 3
	})
	got := workflow(t, store, created.ID)
	running := 0
	for _, task := range got.Tasks {
		if task.State == model.Running {
			running++
		}
	}
	if running != 3 || w.Active() != 3 {
		t.Fatalf("concurrency exceeded or missed: running=%d active=%d", running, w.Active())
	}
	if err := w.Run(context.Background()); err == nil {
		t.Fatal("concurrent Run accepted")
	}
	stop()
	if w.Active() != 0 {
		t.Fatal("active executions survived shutdown")
	}
	nodes, _ := store.Nodes(context.Background())
	if len(nodes) != 0 {
		t.Fatal("stopped worker remained registered")
	}
	eventually(t, "abandoned claims becoming retryable", func() bool {
		if err := store.Reap(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, task := range workflow(t, store, created.ID).Tasks {
			if task.State != model.Pending {
				return false
			}
		}
		return true
	})
	retried, err := store.Claim(context.Background(), "replacement", time.Second)
	if err != nil || retried == nil || retried.Attempt != 2 {
		t.Fatalf("claim did not recover: %+v, %v", retried, err)
	}
}

func TestLeaseIsRenewedAcrossLongExecution(t *testing.T) {
	store := memory.New()
	task := spec("long")
	task.Steps = 10
	task.StepMS = 50
	created := create(t, store, task)
	cfg := config("renewing", 1)
	cfg.LeaseDuration = 150 * time.Millisecond
	start(t, store, cfg)
	eventually(t, "execution longer than lease completing", func() bool {
		return workflow(t, store, created.ID).State.Terminal()
	})
	got := workflow(t, store, created.ID).Tasks[0]
	if got.State != model.Succeeded || got.Attempt != 1 || got.Tokens != 30 {
		t.Fatalf("lease expired during healthy execution: %+v", got)
	}
}

func TestWorkflowCancellationStopsRunningAgent(t *testing.T) {
	store := memory.New()
	task := spec("runaway")
	task.Runaway = true
	task.StepMS = 1000
	created := create(t, store, task)
	var completions atomic.Int64
	cfg := config("cancellable", 1)
	cfg.OnResult = func(model.Task, engine.Result) { completions.Add(1) }
	w, _ := start(t, store, cfg)
	eventually(t, "agent running", func() bool { return w.Active() == 1 })
	if err := store.Cancel(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent stopping after cancellation", func() bool { return w.Active() == 0 })
	got := workflow(t, store, created.ID).Tasks[0]
	if got.State != model.Cancelled || got.Reason != "user_cancelled" || completions.Load() != 0 {
		t.Fatalf("worker overwrote workflow cancellation: %+v", got)
	}
}

type unavailableLease struct {
	queue.Store
	err      error
	renewals atomic.Int64
	finishes atomic.Int64
}

func (s *unavailableLease) Renew(ctx context.Context, task model.Task, lease time.Duration) (bool, error) {
	s.renewals.Add(1)
	if s.err == context.DeadlineExceeded {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return false, s.err
}
func (s *unavailableLease) Finish(ctx context.Context, task model.Task, state model.State, tokens int, reason string) (bool, error) {
	s.finishes.Add(1)
	return s.Store.Finish(ctx, task, state, tokens, reason)
}

func TestLostOrUnavailableLeaseStopsExecutionWithoutFinishing(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"claim lost", nil}, {"database unavailable", errors.New("database unavailable")}, {"database timeout", context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &unavailableLease{Store: memory.New(), err: test.err}
			task := spec("runaway")
			task.StepMS = 1000
			task.Runaway = true
			create(t, store, task)
			w, stop := start(t, store, config("lease-loss", 1))
			eventually(t, "lease failure stopping agent", func() bool { return store.renewals.Load() > 0 && w.Active() == 0 })
			stop()
			if store.finishes.Load() != 0 {
				t.Fatal("worker tried to finish without a confirmed lease")
			}
		})
	}
}

func TestBreakerFailureIsTerminalAndSkipsDependents(t *testing.T) {
	store := memory.New()
	root := spec("runaway")
	root.Runaway = true
	root.TokenLimit = 3
	root.MaxAttempts = 5
	child := spec("child")
	child.DependsOn = []string{"runaway"}
	created := create(t, store, root, child)
	start(t, store, config("breaker", 2))
	eventually(t, "breaker failure becoming terminal", func() bool { return workflow(t, store, created.ID).State.Terminal() })
	got := workflow(t, store, created.ID)
	if got.State != model.Failed || got.Tasks[0].Reason != engine.ReasonTokenLimit || got.Tasks[0].Attempt != 1 || got.Tasks[1].State != model.Skipped || got.Tasks[1].Attempt != 0 {
		t.Fatalf("breaker result was retried or dependency executed: %+v", got)
	}
}

func TestInvalidWorkerConfiguration(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Concurrency = -1 },
		func(c *Config) { c.Concurrency = 4097 },
		func(c *Config) { c.PollInterval = -time.Second },
		func(c *Config) { c.StoreTimeout = c.LeaseDuration },
		func(c *Config) { c.RenewInterval = c.LeaseDuration },
		func(c *Config) { c.HeartbeatInterval = 10 * time.Second },
	} {
		cfg := config("invalid", 1)
		mutate(&cfg)
		if _, err := New(memory.New(), cfg); err == nil {
			t.Fatalf("accepted invalid config: %+v", cfg)
		}
	}
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("accepted nil store")
	}
	if _, err := New(memory.New(), Config{}); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}
