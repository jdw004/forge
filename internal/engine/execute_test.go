package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"forge/internal/model"
)

func task() model.TaskSpec {
	return model.TaskSpec{
		ID: "agent", Steps: 3, StepMS: 1, TokensPerStep: 4,
		TokenLimit: 12, RuntimeMS: 2000, MaxAttempts: 1,
	}
}

func TestTokenBudget(t *testing.T) {
	tests := []struct {
		name          string
		limit         int
		runaway       bool
		state         model.State
		tokens, steps int
		reason        string
	}{
		{"exact budget succeeds", 12, false, model.Succeeded, 12, 3, ""},
		{"spare budget succeeds", 20, false, model.Succeeded, 12, 3, ""},
		{"partial step rejected", 10, false, model.Failed, 8, 2, ReasonTokenLimit},
		{"first step rejected", 3, false, model.Failed, 0, 0, ReasonTokenLimit},
		{"runaway stopped at budget", 12, true, model.Failed, 12, 3, ReasonTokenLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := task()
			spec.TokenLimit, spec.Runaway = tt.limit, tt.runaway
			got := Execute(context.Background(), spec)
			if got.State != tt.state || got.Tokens != tt.tokens || got.Steps != tt.steps || got.Reason != tt.reason {
				t.Fatalf("unexpected result: %+v", got)
			}
			if got.Elapsed <= 0 {
				t.Fatal("elapsed time was not recorded")
			}
		})
	}
}

func TestRuntimeBreakerInterruptsLongStep(t *testing.T) {
	spec := task()
	spec.Runaway = true
	spec.RuntimeMS = 40
	spec.StepMS = 1000
	spec.TokenLimit = 1000000
	got := Execute(context.Background(), spec)
	if got.State != model.Failed || got.Reason != ReasonRuntimeLimit || got.Tokens != 0 || got.Steps != 0 {
		t.Fatalf("runtime breaker did not interrupt the step: %+v", got)
	}
	if got.Elapsed < 40*time.Millisecond || got.Elapsed >= 750*time.Millisecond {
		t.Fatalf("expected deadline to interrupt a 1s step near 40ms, got %s", got.Elapsed)
	}
}

func TestRunawayRuntimeBreakerAfterProgress(t *testing.T) {
	spec := task()
	spec.Runaway = true
	spec.RuntimeMS = 100
	spec.TokenLimit = 1000000
	got := Execute(context.Background(), spec)
	if got.State != model.Failed || got.Reason != ReasonRuntimeLimit || got.Steps < 1 {
		t.Fatalf("runaway did not stop on runtime after making progress: %+v", got)
	}
	if got.Elapsed >= 3*time.Second {
		t.Fatalf("runaway took %s to terminate", got.Elapsed)
	}
	if got.Tokens != got.Steps*spec.TokensPerStep {
		t.Fatalf("incorrect token accounting: %+v", got)
	}
}

func TestCancellation(t *testing.T) {
	t.Run("cancelled before execution", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got := Execute(ctx, task())
		if got.State != model.Cancelled || got.Reason != ReasonCancelled || got.Tokens != 0 {
			t.Fatalf("cancelled task executed: %+v", got)
		}
	})
	t.Run("cancellation interrupts waiting step", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		spec := task()
		spec.StepMS = 1000
		timer := time.AfterFunc(30*time.Millisecond, cancel)
		defer timer.Stop()
		got := Execute(ctx, spec)
		if got.State != model.Cancelled || got.Reason != ReasonCancelled || got.Tokens != 0 || got.Elapsed >= 750*time.Millisecond {
			t.Fatalf("cancellation failed to interrupt work: %+v", got)
		}
	})
	t.Run("parent deadline is not a runtime breaker", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		spec := task()
		spec.StepMS = 1000
		got := Execute(ctx, spec)
		if got.State != model.Cancelled || got.Reason != ReasonCancelled {
			t.Fatalf("parent deadline misclassified as a breaker: %+v", got)
		}
	})
}

func TestInvalidExecutionSettingsFailClosed(t *testing.T) {
	for _, mutate := range []func(*model.TaskSpec){
		func(s *model.TaskSpec) { s.TokenLimit = 0 },
		func(s *model.TaskSpec) { s.TokensPerStep = -1 },
		func(s *model.TaskSpec) { s.RuntimeMS = 0 },
		func(s *model.TaskSpec) { s.StepMS = 0 },
		func(s *model.TaskSpec) { s.Steps = 0 },
	} {
		spec := task()
		mutate(&spec)
		got := Execute(context.Background(), spec)
		if got.State != model.Failed || got.Reason != ReasonInvalidTask || got.Tokens != 0 || got.Steps != 0 {
			t.Fatalf("invalid execution was accepted: %+v", got)
		}
	}
}

func TestConcurrentExecutionsHaveIndependentBudgets(t *testing.T) {
	const count = 64
	results := make(chan Result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec := task()
			spec.Runaway = true
			results <- Execute(context.Background(), spec)
		}()
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got.State != model.Failed || got.Reason != ReasonTokenLimit || got.Tokens != 12 || got.Steps != 3 {
			t.Fatalf("concurrent execution leaked budget state: %+v", got)
		}
	}
}
