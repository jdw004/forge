// Package worker coordinates bounded execution against a shared task queue.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"forge/internal/engine"
	"forge/internal/model"
	"forge/internal/queue"
)

type Config struct {
	ID                string
	Concurrency       int
	LeaseDuration     time.Duration
	RenewInterval     time.Duration
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	ReapInterval      time.Duration
	StoreTimeout      time.Duration
	Logger            *slog.Logger
	// OnResult observes durably accepted results only. It must be safe for
	// concurrent calls and return promptly; metrics exporters can hook in here.
	OnResult func(model.Task, engine.Result)
}

type Worker struct {
	store   queue.Store
	cfg     Config
	active  atomic.Int64
	running atomic.Bool
}

func New(store queue.Store, cfg Config) (*Worker, error) {
	if store == nil {
		return nil, errors.New("worker requires a store")
	}
	if cfg.ID == "" {
		cfg.ID = "worker-" + model.ID()
	}
	if len(cfg.ID) > 128 {
		return nil, errors.New("worker ID must be at most 128 bytes")
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 256
	}
	if cfg.Concurrency < 1 || cfg.Concurrency > 4096 {
		return nil, errors.New("worker concurrency must be 1–4096")
	}
	defaults := []struct {
		value    *time.Duration
		fallback time.Duration
	}{
		{&cfg.LeaseDuration, 5 * time.Second},
		{&cfg.RenewInterval, 500 * time.Millisecond},
		{&cfg.PollInterval, 100 * time.Millisecond},
		{&cfg.HeartbeatInterval, 2 * time.Second},
		{&cfg.ReapInterval, time.Second},
		{&cfg.StoreTimeout, time.Second},
	}
	for _, d := range defaults {
		if *d.value == 0 {
			*d.value = d.fallback
		}
		if *d.value < 0 {
			return nil, errors.New("worker intervals must be positive")
		}
	}
	if cfg.RenewInterval >= cfg.LeaseDuration || cfg.StoreTimeout >= cfg.LeaseDuration-cfg.RenewInterval {
		return nil, errors.New("renew interval plus store timeout must be shorter than the lease")
	}
	if cfg.HeartbeatInterval >= 5*time.Second {
		return nil, errors.New("heartbeat interval must be less than 5 seconds")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{store: store, cfg: cfg}, nil
}

func (w *Worker) ID() string  { return w.cfg.ID }
func (w *Worker) Active() int { return int(w.active.Load()) }

// Run blocks until cancellation and joins every execution and renewal goroutine.
// Shutdown leaves interrupted claims to expire, allowing another node to retry
// them within MaxAttempts. Breaker failures are terminal and are never retried.
// Store implementations must honor context deadlines on all I/O operations.
func (w *Worker) Run(ctx context.Context) error {
	if !w.running.CompareAndSwap(false, true) {
		return errors.New("worker is already running")
	}
	defer w.running.Store(false)
	if ctx.Err() != nil {
		return nil
	}
	if err := w.heartbeat(ctx, w.cfg.Concurrency); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.maintain(ctx) }()
	for i := 0; i < w.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.slot(ctx) }()
	}
	wg.Wait()
	// Use a fresh, bounded context because the run context is now cancelled.
	if err := w.heartbeat(context.Background(), 0); err != nil {
		w.cfg.Logger.Warn("worker retirement failed; heartbeat will expire", "worker", w.ID(), "error", err)
	}
	return nil
}

func (w *Worker) heartbeat(ctx context.Context, capacity int) error {
	op, cancel := context.WithTimeout(ctx, w.cfg.StoreTimeout)
	defer cancel()
	return w.store.Heartbeat(op, model.Node{ID: w.ID(), Capacity: capacity, Active: w.Active()})
}

func (w *Worker) maintain(ctx context.Context) {
	heartbeat := time.NewTicker(w.cfg.HeartbeatInterval)
	reap := time.NewTicker(w.cfg.ReapInterval)
	defer heartbeat.Stop()
	defer reap.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := w.heartbeat(ctx, w.cfg.Concurrency); err != nil && ctx.Err() == nil {
				w.cfg.Logger.Warn("worker heartbeat failed", "worker", w.ID(), "error", err)
			}
		case <-reap.C:
			op, cancel := context.WithTimeout(ctx, w.cfg.StoreTimeout)
			err := w.store.Reap(op)
			cancel()
			if err != nil && ctx.Err() == nil {
				w.cfg.Logger.Warn("lease recovery failed", "worker", w.ID(), "error", err)
			}
		}
	}
}

func (w *Worker) slot(ctx context.Context) {
	for ctx.Err() == nil {
		op, cancel := context.WithTimeout(ctx, w.cfg.StoreTimeout)
		task, err := w.store.Claim(op, w.ID(), w.cfg.LeaseDuration)
		cancel()
		if err != nil || task == nil {
			if err != nil && ctx.Err() == nil {
				w.cfg.Logger.Warn("task claim failed", "worker", w.ID(), "error", err)
			}
			timer := time.NewTimer(w.cfg.PollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		w.active.Add(1)
		w.execute(ctx, *task)
		w.active.Add(-1)
	}
}

func (w *Worker) execute(ctx context.Context, task model.Task) {
	execution, cancel := context.WithCancel(ctx)
	defer cancel()
	var lost atomic.Bool
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(w.cfg.RenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-execution.Done():
				return
			case <-ticker.C:
				op, stop := context.WithTimeout(execution, w.cfg.StoreTimeout)
				ok, err := w.store.Renew(op, task, w.cfg.LeaseDuration)
				stop()
				if execution.Err() != nil {
					return
				}
				if err != nil || !ok {
					lost.Store(true)
					cancel()
					if err != nil {
						w.cfg.Logger.Warn("lease renewal failed; execution stopped", "task", task.ID, "worker", w.ID(), "error", err)
					}
					return
				}
			}
		}
	}()
	result := engine.Execute(execution, task.TaskSpec)
	cancel()
	<-renewed
	if ctx.Err() != nil || lost.Load() {
		return
	}

	op, stop := context.WithTimeout(ctx, w.cfg.StoreTimeout)
	ok, err := w.store.Finish(op, task, result.State, result.Tokens, result.Reason)
	stop()
	if err != nil {
		w.cfg.Logger.Error("task completion failed; lease will recover", "task", task.ID, "worker", w.ID(), "error", err)
		return
	}
	if ok && w.cfg.OnResult != nil {
		w.cfg.OnResult(task, result)
	}
}
