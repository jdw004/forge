package loadtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"forge/internal/engine"
	"forge/internal/memory"
	"forge/internal/model"
	"forge/internal/worker"
)

type observer struct {
	mu                     sync.Mutex
	active, peak, finished int
	latencies              []time.Duration
}

func (o *observer) StartTask(ctx context.Context, _ model.Task) (context.Context, func(engine.Result, bool)) {
	o.mu.Lock()
	o.active++
	if o.active > o.peak {
		o.peak = o.active
	}
	o.mu.Unlock()
	return ctx, func(result engine.Result, _ bool) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.active--
		o.finished++
		o.latencies = append(o.latencies, result.Elapsed)
	}
}

// RunMemory runs real worker pools and the built-in executor in one process.
// It is not a distributed-node or PostgreSQL benchmark.
func RunMemory(parent context.Context, options Options) (Report, error) {
	spec, err := Spec(options)
	if err != nil {
		return Report{}, err
	}
	return runMemory(parent, options, spec)
}

func runMemory(parent context.Context, options Options, spec model.Spec) (Report, error) {
	report := newReport(options, "memory", spec)
	report.ExecutionModel = "single_process_worker_pools"
	report.WorkerPools = options.Workers
	report.ConfiguredCapacity = options.Workers * options.Concurrency
	report.ConcurrencySource = "observer_active_attempts_including_acknowledgement"
	report.LatencySource = "executor_elapsed"
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	store := memory.New()
	measure := &observer{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var wg sync.WaitGroup
	failures := make(chan error, options.Workers)
	for i := 0; i < options.Workers; i++ {
		w, err := worker.New(store, worker.Config{ID: fmt.Sprintf("load-%02d", i+1), Concurrency: options.Concurrency, Observer: measure, Logger: logger})
		if err != nil {
			cancel()
			wg.Wait()
			return report, err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Run(ctx); err != nil {
				failures <- err
				cancel()
			}
		}()
	}
	stop := func() { cancel(); wg.Wait() }
	defer stop()
	// Register every pool before submitting work; startup is outside timed execution.
	ready := time.NewTicker(5 * time.Millisecond)
	defer ready.Stop()
	for {
		nodes, _ := store.Nodes(ctx)
		if len(nodes) == options.Workers {
			break
		}
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case <-ready.C:
		}
	}
	report.StartedAt = time.Now().UTC()
	started := time.Now()
	workflow, _, err := store.Create(ctx, spec, "")
	if err != nil {
		return report, err
	}
	report.WorkflowID = workflow.ID
	poll := time.NewTicker(options.PollInterval)
	defer poll.Stop()
wait:
	for {
		measure.mu.Lock()
		finished := measure.finished
		measure.mu.Unlock()
		if finished >= options.Tasks {
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break wait
		case <-poll.C:
		}
	}
	stop()
	elapsed := time.Since(started)
	workflow, _ = store.Get(context.Background(), workflow.ID)
	report.summarize(workflow, elapsed)
	measure.mu.Lock()
	report.PeakActive = measure.peak
	report.Latency = latency(measure.latencies)
	measure.mu.Unlock()
	report.Cleanup = "embedded_workers_stopped"
	select {
	case failure := <-failures:
		err = failure
	default:
	}
	if err == nil && !report.Passed {
		err = errors.New("one or more task outcomes did not match the scenario")
	}
	if err != nil {
		report.Passed = false
		report.Error = err.Error()
	}
	return report, err
}
