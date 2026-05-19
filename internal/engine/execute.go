// Package engine executes the built-in simulated agent with per-attempt token
// and runtime circuit breakers. No external service or credentials are required.
package engine

import (
	"context"
	"errors"
	"time"

	"forge/internal/model"
)

const (
	ReasonTokenLimit   = "token_limit"
	ReasonRuntimeLimit = "runtime_limit"
	ReasonCancelled    = "cancelled"
	ReasonInvalidTask  = "invalid_task"
)

var errRuntimeLimit = errors.New("task runtime budget exhausted")

// Result is the outcome of a single attempt. Tokens counts completed simulated
// steps; a rejected or interrupted step consumes no tokens in this simulator.
type Result struct {
	State   model.State   `json:"state"`
	Tokens  int           `json:"tokens"`
	Steps   int           `json:"steps"`
	Reason  string        `json:"reason,omitempty"`
	Elapsed time.Duration `json:"elapsed_ns"`
}

// Execute runs synchronously, so returning also means the simulated agent has
// stopped. Every step observes cancellation, including while it is waiting.
//
// Runaway agents ignore the requested step count, but never the budgets. Limits
// apply per attempt; the worker must not retry token/runtime breaker failures.
// These guarantees depend on this cooperative executor. Arbitrary untrusted
// code would require process/container isolation and an external kill mechanism.
func Execute(parent context.Context, spec model.TaskSpec) (result Result) {
	started := time.Now()
	defer func() { result.Elapsed = time.Since(started) }()

	// Validate execution settings even when called without the HTTP API. The
	// queue has already resolved dependencies; they are not executor inputs.
	validation := spec
	validation.DependsOn = nil
	if err := model.Validate(model.Spec{Name: "execution", Tasks: []model.TaskSpec{validation}}); err != nil {
		result.State, result.Reason = model.Failed, ReasonInvalidTask
		return
	}

	ctx, cancel := context.WithDeadlineCause(parent, started.Add(time.Duration(spec.RuntimeMS)*time.Millisecond), errRuntimeLimit)
	defer cancel()

	stopped := func() bool {
		if ctx.Err() == nil {
			return false
		}
		result.State, result.Reason = model.Cancelled, ReasonCancelled
		if errors.Is(context.Cause(ctx), errRuntimeLimit) {
			result.State, result.Reason = model.Failed, ReasonRuntimeLimit
		}
		return true
	}

	for spec.Runaway || result.Steps < spec.Steps {
		if stopped() {
			return
		}
		// Check before starting work. Subtraction avoids overflowing an addition
		// and ensures that no accepted step can exceed the token budget.
		if spec.TokensPerStep > spec.TokenLimit-result.Tokens {
			result.State, result.Reason = model.Failed, ReasonTokenLimit
			return
		}
		timer := time.NewTimer(time.Duration(spec.StepMS) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			stopped()
			return
		case <-timer.C:
			if stopped() {
				return
			}
			result.Steps++
			result.Tokens += spec.TokensPerStep
		}
	}
	if stopped() {
		return
	}
	result.State = model.Succeeded
	return
}
