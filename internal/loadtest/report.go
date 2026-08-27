// Package loadtest runs reproducible simulated workloads and reports observed
// results. A slot count is never treated as measured concurrency.
package loadtest

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"time"

	"forge/internal/engine"
	"forge/internal/model"
)

type Options struct {
	Tasks        int
	Workers      int
	Concurrency  int
	Scenario     string
	TaskDuration time.Duration
	RuntimeLimit time.Duration
	Timeout      time.Duration
	PollInterval time.Duration
}

func Defaults() Options {
	return Options{Tasks: 2200, Workers: 10, Concurrency: 256, Scenario: "batch", TaskDuration: 5 * time.Second, RuntimeLimit: time.Second, Timeout: 90 * time.Second, PollInterval: 100 * time.Millisecond}
}

func (o Options) Validate() error {
	if o.Tasks < 1 || o.Tasks > 5000 {
		return errors.New("tasks must be 1–5000")
	}
	if o.Workers < 1 || o.Workers > 10 || o.Concurrency < 1 || o.Concurrency > 4096 || o.Workers*o.Concurrency > 4096 {
		return errors.New("use 1–10 workers and 1–4096 slots, with at most 4096 total slots")
	}
	if o.Scenario != "batch" && o.Scenario != "token" && o.Scenario != "runtime" {
		return errors.New("scenario must be batch, token, or runtime")
	}
	if o.TaskDuration < time.Millisecond || o.TaskDuration > time.Minute || o.TaskDuration%time.Millisecond != 0 {
		return errors.New("task duration must be 1ms–1m in whole milliseconds")
	}
	if o.RuntimeLimit < 10*time.Millisecond || o.RuntimeLimit > time.Minute || o.RuntimeLimit%time.Millisecond != 0 {
		return errors.New("runtime limit must be 10ms–1m in whole milliseconds")
	}
	if o.Timeout <= 0 || o.Timeout > 30*time.Minute {
		return errors.New("timeout must be greater than zero and at most 30m")
	}
	if o.PollInterval < 10*time.Millisecond || o.PollInterval > 5*time.Second {
		return errors.New("poll interval must be 10ms–5s")
	}
	return nil
}

func Spec(o Options) (model.Spec, error) {
	if err := o.Validate(); err != nil {
		return model.Spec{}, err
	}
	duration := int(o.TaskDuration.Milliseconds())
	steps := (duration + 999) / 1000
	stepMS := (duration + steps - 1) / steps
	s := model.Spec{Name: fmt.Sprintf("Forge load: %s (%d tasks)", o.Scenario, o.Tasks), Tasks: make([]model.TaskSpec, o.Tasks)}
	for i := range s.Tasks {
		t := model.TaskSpec{ID: fmt.Sprintf("task-%05d", i+1), Steps: steps, StepMS: stepMS, TokensPerStep: 10, TokenLimit: steps * 10, RuntimeMS: duration*2 + 5000, MaxAttempts: 1}
		if o.Scenario == "token" {
			t.Runaway = true
			t.StepMS = 25
			t.TokenLimit = 50
			t.RuntimeMS = 3000
		}
		if o.Scenario == "runtime" {
			t.Runaway = true
			t.StepMS = 1000
			t.TokenLimit = 1000000
			t.RuntimeMS = int(o.RuntimeLimit.Milliseconds())
		}
		s.Tasks[i] = t
	}
	return s, model.Validate(s)
}

type Latency struct {
	Samples int     `json:"samples"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	MaxMS   float64 `json:"max_ms"`
}

type Report struct {
	StartedAt                time.Time           `json:"started_at"`
	Backend                  string              `json:"backend"`
	ExecutionModel           string              `json:"execution_model"`
	Scenario                 string              `json:"scenario"`
	TasksRequested           int                 `json:"tasks_requested"`
	WorkflowID               string              `json:"workflow_id,omitempty"`
	SubmissionKey            string              `json:"submission_key,omitempty"`
	WorkerPools              int                 `json:"configured_worker_pools,omitempty"`
	ConfiguredCapacity       int                 `json:"configured_capacity,omitempty"`
	WorkersObserved          int                 `json:"workers_observed"`
	PeakActive               int                 `json:"peak_active_attempts"`
	ConcurrencySource        string              `json:"concurrency_source"`
	LatencySource            string              `json:"latency_source"`
	PollIntervalMS           int64               `json:"poll_interval_ms"`
	ElapsedMS                float64             `json:"elapsed_ms"`
	TerminalResultsPerSecond float64             `json:"terminal_results_per_second"`
	States                   map[model.State]int `json:"states"`
	Reasons                  map[string]int      `json:"reasons"`
	Unexpected               int                 `json:"unexpected_outcomes"`
	Passed                   bool                `json:"passed"`
	Latency                  Latency             `json:"attempt_latency"`
	Cleanup                  string              `json:"cleanup,omitempty"`
	Error                    string              `json:"error,omitempty"`
	GoVersion                string              `json:"driver_go_version"`
	Platform                 string              `json:"driver_platform"`
	TaskSpec                 model.TaskSpec      `json:"task_spec"`
}

func newReport(o Options, backend string, spec model.Spec) Report {
	return Report{Backend: backend, Scenario: o.Scenario, TasksRequested: o.Tasks, PollIntervalMS: o.PollInterval.Milliseconds(), States: map[model.State]int{}, Reasons: map[string]int{}, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, TaskSpec: spec.Tasks[0]}
}

func latency(values []time.Duration) Latency {
	if len(values) == 0 {
		return Latency{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return Latency{Samples: len(values), P50MS: ms(values[int(math.Ceil(.5*float64(len(values))))-1]), P95MS: ms(values[int(math.Ceil(.95*float64(len(values))))-1]), MaxMS: ms(values[len(values)-1])}
}

func (r *Report) summarize(workflow model.Workflow, elapsed time.Duration) {
	r.ElapsedMS = float64(elapsed) / float64(time.Millisecond)
	r.States = map[model.State]int{}
	r.Reasons = map[string]int{}
	r.Unexpected = 0
	workers := map[string]bool{}
	terminalCount := 0
	for _, task := range workflow.Tasks {
		r.States[task.State]++
		if task.Reason != "" {
			r.Reasons[task.Reason]++
		}
		if task.WorkerID != "" {
			workers[task.WorkerID] = true
		}
		if task.State.Terminal() {
			terminalCount++
		}
		expected := task.State == model.Succeeded
		if r.Scenario == "token" {
			expected = task.State == model.Failed && task.Reason == engine.ReasonTokenLimit
		}
		if r.Scenario == "runtime" {
			expected = task.State == model.Failed && task.Reason == engine.ReasonRuntimeLimit
		}
		if !expected || task.Attempt != 1 {
			r.Unexpected++
		}
	}
	r.WorkersObserved = len(workers)
	r.Passed = len(workflow.Tasks) == r.TasksRequested && r.Unexpected == 0
	if elapsed > 0 {
		r.TerminalResultsPerSecond = float64(terminalCount) / elapsed.Seconds()
	}
}
