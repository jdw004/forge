package telemetry

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/engine"
	"forge/internal/model"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func value(t *testing.T, monitor *Telemetry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := monitor.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := len(metric.GetLabel()) == len(labels)
			for _, pair := range metric.GetLabel() {
				if labels[pair.GetName()] != pair.GetValue() {
					matched = false
				}
			}
			if matched {
				if metric.Gauge != nil {
					return metric.Gauge.GetValue()
				}
				if metric.Counter != nil {
					return metric.Counter.GetValue()
				}
			}
		}
	}
	return 0
}

func TestTaskAccountingAndIsolatedRegistries(t *testing.T) {
	monitor := Local()
	other := Local()
	monitor.SetCapacity(256)
	_, finish := monitor.StartTask(context.Background(), model.Task{WorkflowID: "private-workflow", TaskSpec: model.TaskSpec{ID: "private-task"}})
	if value(t, monitor, "forge_task_active", nil) != 1 {
		t.Fatal("attempt did not become active")
	}
	result := engine.Result{State: model.Failed, Reason: engine.ReasonTokenLimit, Tokens: 10}
	finish(result, true)
	finish(result, true) // Duplicate notifications must not double-count or underflow.
	if value(t, monitor, "forge_task_active", nil) != 0 || value(t, monitor, "forge_worker_capacity", nil) != 256 {
		t.Fatal("incorrect active/capacity gauges")
	}
	if value(t, monitor, "forge_task_tokens_total", nil) != 10 || value(t, other, "forge_task_tokens_total", nil) != 0 {
		t.Fatal("token accounting leaked or doubled")
	}
	if value(t, monitor, "forge_tasks_completed_total", map[string]string{"state": "failed"}) != 1 {
		t.Fatal("committed completion was not recorded")
	}
	if value(t, monitor, "forge_breaker_trips_total", map[string]string{"reason": "token_limit"}) != 1 {
		t.Fatal("breaker trip was not recorded")
	}
	if value(t, monitor, "forge_task_attempts_total", map[string]string{"state": "failed", "reason": "token_limit", "committed": "true"}) != 1 {
		t.Fatal("attempt was not recorded")
	}
	w := httptest.NewRecorder()
	monitor.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "private-task") || strings.Contains(w.Body.String(), "private-workflow") {
		t.Fatal("metric scrape failed or exposed task identifiers")
	}
}

func TestAbandonedAttemptsAreNotCompletions(t *testing.T) {
	monitor := Local()
	_, finish := monitor.StartTask(context.Background(), model.Task{})
	finish(engine.Result{State: model.Succeeded, Tokens: 3}, false)
	if value(t, monitor, "forge_tasks_completed_total", map[string]string{"state": "succeeded"}) != 0 {
		t.Fatal("uncommitted result counted as completion")
	}
	if value(t, monitor, "forge_task_attempts_total", map[string]string{"state": "succeeded", "reason": "none", "committed": "false"}) != 1 {
		t.Fatal("abandoned attempt was lost")
	}
	if value(t, monitor, "forge_task_tokens_total", nil) != 3 {
		t.Fatal("abandoned attempt lost consumed tokens")
	}
}

func TestConcurrentAttemptAccounting(t *testing.T) {
	monitor := Local()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, end := monitor.StartTask(context.Background(), model.Task{})
			end(engine.Result{State: model.Succeeded, Tokens: 2}, true)
		}()
	}
	wg.Wait()
	if value(t, monitor, "forge_task_active", nil) != 0 || value(t, monitor, "forge_task_tokens_total", nil) != 128 || value(t, monitor, "forge_tasks_completed_total", map[string]string{"state": "succeeded"}) != 64 {
		t.Fatal("concurrent metrics were inconsistent")
	}
}

func recording(t *testing.T) (*Telemetry, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	monitor := newTelemetry(provider, provider.Shutdown)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := monitor.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return monitor, exporter
}

func TestTaskSpansRecordAttemptOutcome(t *testing.T) {
	monitor, exporter := recording(t)
	_, end := monitor.StartTask(context.Background(), model.Task{WorkflowID: "workflow-1", WorkerID: "node-1", Attempt: 2, TaskSpec: model.TaskSpec{ID: "task-1"}})
	end(engine.Result{State: model.Failed, Reason: engine.ReasonRuntimeLimit}, true)
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "forge.task.execute" || spans[0].Status.Code != codes.Error {
		t.Fatalf("unexpected task spans: %+v", spans)
	}
	attrs := map[string]string{}
	for _, attr := range spans[0].Attributes {
		attrs[string(attr.Key)] = attr.Value.Emit()
	}
	if attrs["forge.workflow.id"] != "workflow-1" || attrs["forge.task.reason"] != "runtime_limit" || attrs["forge.task.committed"] != "true" {
		t.Fatalf("missing task attributes: %+v", attrs)
	}
}

func TestHTTPUsesRouteTemplatesAndPropagatesTraceContext(t *testing.T) {
	monitor, exporter := recording(t)
	router := chi.NewRouter()
	router.Use(monitor.HTTPMiddleware)
	router.Get("/v1/workflows/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	for _, id := range []string{"private-one", "private-two"} {
		request := httptest.NewRequest("GET", "/v1/workflows/"+id+"?token=secret", nil)
		request.Header.Set("Authorization", "Bearer private-token")
		request.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
		router.ServeHTTP(httptest.NewRecorder(), request)
	}
	if value(t, monitor, "forge_http_requests_total", map[string]string{"method": "GET", "route": "/v1/workflows/{id}", "status": "503"}) != 2 {
		t.Fatal("requests were not grouped by route template")
	}
	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected two server spans, got %d", len(spans))
	}
	for _, span := range spans {
		if span.Name != "GET /v1/workflows/{id}" || span.Parent.TraceID().String() != "0123456789abcdef0123456789abcdef" || span.Status.Code != codes.Error {
			t.Fatalf("invalid server span: %+v", span)
		}
		for _, attr := range span.Attributes {
			if strings.Contains(attr.Value.Emit(), "private") || strings.Contains(attr.Value.Emit(), "secret") {
				t.Fatal("request secrets leaked into span attributes")
			}
		}
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
	if len(exporter.GetSpans()) != 2 {
		t.Fatal("metric scrape emitted a span")
	}
}

func TestDisabledTracingAndInvalidConfiguration(t *testing.T) {
	monitor, err := New(context.Background(), Config{SampleRatio: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	if err := monitor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{
		{SampleRatio: math.NaN()}, {SampleRatio: -1}, {SampleRatio: 2},
		{Endpoint: "ftp://collector"}, {Endpoint: "http://user:password@collector"}, {Endpoint: "https://collector?token=private"},
	} {
		if _, err := New(context.Background(), cfg); err == nil {
			t.Fatal("invalid telemetry config accepted")
		}
	}
}
