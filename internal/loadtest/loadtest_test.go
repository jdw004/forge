package loadtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"forge/internal/api"
	"forge/internal/memory"
	"forge/internal/model"
	"forge/internal/worker"
)

func smallOptions() Options {
	o := Defaults()
	o.Tasks, o.Workers, o.Concurrency = 12, 2, 8
	o.TaskDuration, o.RuntimeLimit = 30*time.Millisecond, 20*time.Millisecond
	o.Timeout, o.PollInterval = 5*time.Second, 10*time.Millisecond
	return o
}

func TestMemoryScenarios(t *testing.T) {
	for _, scenario := range []string{"batch", "token", "runtime"} {
		t.Run(scenario, func(t *testing.T) {
			o := smallOptions()
			o.Scenario = scenario
			r, err := RunMemory(context.Background(), o)
			if err != nil || !r.Passed || r.Latency.Samples != o.Tasks || r.PeakActive < 1 || r.PeakActive > o.Tasks || r.Cleanup != "embedded_workers_stopped" {
				t.Fatalf("report=%+v, err=%v", r, err)
			}
		})
	}
}

func TestMemoryDeadline(t *testing.T) {
	o := smallOptions()
	o.Timeout, o.TaskDuration = 150*time.Millisecond, time.Second
	r, err := RunMemory(context.Background(), o)
	if !errors.Is(err, context.DeadlineExceeded) || r.Passed || r.Cleanup != "embedded_workers_stopped" {
		t.Fatalf("report=%+v, err=%v", r, err)
	}
}

func TestInvalidOptions(t *testing.T) {
	for _, change := range []func(*Options){
		func(o *Options) { o.Tasks = 0 },
		func(o *Options) { o.Tasks = 5001 },
		func(o *Options) { o.Workers = 11 },
		func(o *Options) { o.Concurrency = 4096 },
		func(o *Options) { o.Scenario = "unknown" },
		func(o *Options) { o.TaskDuration = time.Nanosecond },
		func(o *Options) { o.RuntimeLimit = time.Millisecond },
		func(o *Options) { o.Timeout = 0 },
		func(o *Options) { o.PollInterval = time.Millisecond },
	} {
		o := Defaults()
		change(&o)
		if _, err := Spec(o); err == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
}

func TestReportRejectsRetriesAndMissingTasks(t *testing.T) {
	o := smallOptions()
	o.Tasks = 1
	s, _ := Spec(o)
	r := newReport(o, "memory", s)
	r.summarize(model.Workflow{}, time.Second)
	if r.Passed {
		t.Fatal("missing tasks passed")
	}
	r.summarize(model.Workflow{Tasks: []model.Task{{State: model.Succeeded, Attempt: 2}}}, time.Second)
	if r.Passed || r.Unexpected != 1 {
		t.Fatal("retry passed")
	}
	l := latency([]time.Duration{4 * time.Millisecond, time.Millisecond, 3 * time.Millisecond, 2 * time.Millisecond})
	if l.P50MS != 2 || l.P95MS != 4 || l.MaxMS != 4 {
		t.Fatalf("latency=%+v", l)
	}
}

type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	r := httptest.NewRecorder()
	h.handler.ServeHTTP(r, req)
	return r.Result(), nil
}

func TestHTTPScenarios(t *testing.T) {
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := api.New(store, api.Config{Mode: "demo", Token: "test-token", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: handlerTransport{handler}}
	ctx, cancel := context.WithCancel(context.Background())
	w, err := worker.New(store, worker.Config{ID: "http-worker", Concurrency: 16, PollInterval: 10 * time.Millisecond, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for _, scenario := range []string{"batch", "token", "runtime"} {
		t.Run(scenario, func(t *testing.T) {
			o := smallOptions()
			o.Scenario = scenario
			r, err := RunHTTP(ctx, o, "https://forge.example", "test-token", client)
			if err != nil || !r.Passed || r.WorkersObserved != 1 || r.Latency.Samples != o.Tasks || r.SubmissionKey == "" {
				t.Fatalf("report=%+v, err=%v", r, err)
			}
		})
	}
	if _, err := RunHTTP(ctx, smallOptions(), "https://forge.example", "wrong", client); err == nil {
		t.Fatal("accepted invalid token")
	}
	workflows, _ := store.List(ctx, 100)
	if len(workflows) != 3 {
		t.Fatalf("unexpected workflow count: %d", len(workflows))
	}
}

func TestHTTPTimeoutCancelsOnlySubmittedWorkflow(t *testing.T) {
	store := memory.New()
	o := smallOptions()
	s, _ := Spec(o)
	unrelated, _, _ := store.Create(context.Background(), s, "")
	handler, _ := api.New(store, api.Config{Mode: "demo"})
	o.Timeout = 50 * time.Millisecond
	r, err := RunHTTP(context.Background(), o, "https://forge.example", "", &http.Client{Transport: handlerTransport{handler}})
	if !errors.Is(err, context.DeadlineExceeded) || r.Passed || r.Cleanup != "workflow_cancelled" {
		t.Fatalf("report=%+v, err=%v", r, err)
	}
	created, _ := store.Get(context.Background(), r.WorkflowID)
	if created.State != model.Cancelled {
		t.Fatalf("created state=%s", created.State)
	}
	other, _ := store.Get(context.Background(), unrelated.ID)
	if other.State == model.Cancelled {
		t.Fatal("cancelled unrelated work")
	}
}

func TestHTTPRejectsRedirects(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "https://elsewhere.example", http.StatusFound)
	})}}
	if _, err := RunHTTP(context.Background(), smallOptions(), "https://forge.example", "secret", client); err == nil || requests != 1 {
		t.Fatalf("requests=%d, err=%v", requests, err)
	}
}
