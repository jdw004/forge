package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/memory"
	"forge/internal/model"
	"forge/internal/queue"
)

const validWorkflow = `{"name":"demo","tasks":[{"id":"agent","steps":2,"step_ms":10,"tokens_per_step":5,"token_limit":20,"runtime_ms":1000,"max_attempts":2}]}`

func handler(t *testing.T, store queue.Store, cfg Config) http.Handler {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func request(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func expectCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("want status %d; got %d: %s", status, response.Code, response.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code {
		t.Fatalf("want code %s; got %+v", code, body)
	}
	if body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") {
		t.Fatal("missing/mismatched request identifier")
	}
}

func TestWorkflowLifecycleAndSummary(t *testing.T) {
	store := memory.New()
	h := handler(t, store, Config{})
	created := request(h, "POST", "/v1/workflows", validWorkflow, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var workflow model.Workflow
	if err := json.Unmarshal(created.Body.Bytes(), &workflow); err != nil {
		t.Fatal(err)
	}
	location := "/v1/workflows/" + workflow.ID
	if created.Header().Get("Location") != location {
		t.Fatal("missing workflow location")
	}
	if created.Header().Get("Cache-Control") != "no-store" || created.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing response headers")
	}
	claimed, err := store.Claim(context.Background(), "node", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	got := request(h, "GET", location, "", nil)
	if got.Code != http.StatusOK || strings.Contains(got.Body.String(), claimed.Claim) {
		t.Fatalf("detail failed or leaked claim: %s", got.Body.String())
	}
	list := request(h, "GET", "/v1/workflows?limit=1", "", nil)
	var summaries struct {
		Workflows []WorkflowSummary `json:"workflows"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if list.Code != http.StatusOK || len(summaries.Workflows) != 1 || summaries.Workflows[0].Counts[model.Running] != 1 || summaries.Workflows[0].TaskCount != 1 || strings.Contains(list.Body.String(), `"tasks"`) {
		t.Fatalf("incorrect summary: %s", list.Body.String())
	}
	for i := 0; i < 2; i++ {
		cancelled := request(h, "POST", location+"/cancel", "", nil)
		if cancelled.Code != http.StatusNoContent || cancelled.Body.Len() != 0 {
			t.Fatalf("cancel: %d %s", cancelled.Code, cancelled.Body.String())
		}
	}
	got = request(h, "GET", location, "", nil)
	if err := json.Unmarshal(got.Body.Bytes(), &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.State != model.Cancelled || workflow.Tasks[0].Reason != "user_cancelled" {
		t.Fatalf("workflow was not cancelled: %+v", workflow)
	}
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	h := handler(t, memory.New(), Config{})
	headers := map[string]string{"Idempotency-Key": "same-request"}
	first := request(h, "POST", "/v1/workflows", validWorkflow, headers)
	second := request(h, "POST", "/v1/workflows", validWorkflow, headers)
	if first.Code != http.StatusCreated || second.Code != http.StatusOK || first.Header().Get("Location") != second.Header().Get("Location") {
		t.Fatalf("idempotency failed: %s / %s", first.Body.String(), second.Body.String())
	}
	conflict := request(h, "POST", "/v1/workflows", strings.Replace(validWorkflow, `"demo"`, `"other"`, 1), headers)
	expectCode(t, conflict, http.StatusConflict, "idempotency_conflict")
}

func TestConcurrentSubmissionWithSameKey(t *testing.T) {
	store := memory.New()
	h := handler(t, store, Config{})
	const count = 16
	results := make(chan *httptest.ResponseRecorder, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- request(h, "POST", "/v1/workflows", validWorkflow, map[string]string{"Idempotency-Key": "concurrent"})
		}()
	}
	wg.Wait()
	close(results)
	inserts, location := 0, ""
	for response := range results {
		if response.Code == http.StatusCreated {
			inserts++
		} else if response.Code != http.StatusOK {
			t.Fatalf("submission failed: %s", response.Body.String())
		}
		if location != "" && location != response.Header().Get("Location") {
			t.Fatal("duplicate workflows created")
		}
		location = response.Header().Get("Location")
	}
	if inserts != 1 {
		t.Fatalf("expected one creation, got %d", inserts)
	}
}

func TestRejectInvalidSubmissionWithoutEnqueueing(t *testing.T) {
	tests := []struct {
		name, body string
		headers    map[string]string
		status     int
		code       string
	}{
		{"wrong media type", validWorkflow, map[string]string{"Content-Type": "text/plain"}, 415, "unsupported_media_type"},
		{"malformed", "{", nil, 400, "invalid_json"},
		{"unknown field", strings.Replace(validWorkflow, `"name":`, `"extra":true,"name":`, 1), nil, 400, "invalid_json"},
		{"second value", validWorkflow + " {}", nil, 400, "invalid_json"},
		{"null", "null", nil, 400, "invalid_workflow"},
		{"invalid budget", strings.Replace(validWorkflow, `"token_limit":20`, `"token_limit":0`, 1), nil, 400, "invalid_workflow"},
		{"cycle", strings.Replace(validWorkflow, `"id":"agent"`, `"id":"agent","depends_on":["agent"]`, 1), nil, 400, "invalid_workflow"},
		{"large key", validWorkflow, map[string]string{"Idempotency-Key": strings.Repeat("x", 201)}, 400, "invalid_idempotency_key"},
		{"large body", `{"name":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, nil, 413, "body_too_large"},
		{"large trailing whitespace", validWorkflow + strings.Repeat(" ", MaxBodyBytes), nil, 413, "body_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := memory.New()
			h := handler(t, store, Config{})
			expectCode(t, request(h, "POST", "/v1/workflows", test.body, test.headers), test.status, test.code)
			workflows, _ := store.List(context.Background(), 100)
			if len(workflows) != 0 {
				t.Fatal("invalid request mutated the store")
			}
		})
	}
}

func TestAuthenticationAndPublicHealth(t *testing.T) {
	h := handler(t, memory.New(), Config{Token: "test-secret"})
	for _, path := range []string{"/healthz", "/readyz"} {
		if response := request(h, "GET", path, "", nil); response.Code != http.StatusOK {
			t.Fatalf("health requires auth: %d", response.Code)
		}
	}
	for _, header := range []string{"", "Bearer wrong", "Basic test-secret", "Bearer test-secret extra"} {
		response := request(h, "GET", "/v1/nodes", "", map[string]string{"Authorization": header})
		expectCode(t, response, http.StatusUnauthorized, "unauthorized")
		if response.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("missing authentication challenge")
		}
	}
	response := request(h, "GET", "/v1/nodes", "", map[string]string{"Authorization": "bearer test-secret"})
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"nodes":[]}` {
		t.Fatalf("authorized request failed: %s", response.Body.String())
	}
}

type brokenStore struct {
	queue.Store
	err               error
	wait, panicOnList bool
}

func (s brokenStore) Ping(context.Context) error { return s.err }
func (s brokenStore) List(ctx context.Context, limit int) ([]model.Workflow, error) {
	if s.panicOnList {
		panic("private implementation detail")
	}
	if s.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, s.err
}

func TestStoreFailuresAreSanitizedAndBounded(t *testing.T) {
	broken := brokenStore{Store: memory.New(), err: errors.New("database password=private-secret")}
	h := handler(t, broken, Config{})
	response := request(h, "GET", "/v1/workflows", "", nil)
	expectCode(t, response, http.StatusServiceUnavailable, "store_unavailable")
	if strings.Contains(response.Body.String(), "private-secret") {
		t.Fatal("database error leaked")
	}
	expectCode(t, request(h, "GET", "/readyz", "", nil), http.StatusServiceUnavailable, "not_ready")
	if got := request(h, "GET", "/healthz", "", nil); got.Code != http.StatusOK {
		t.Fatal("liveness depends on database")
	}
	h = handler(t, brokenStore{Store: memory.New(), wait: true}, Config{RequestTimeout: 10 * time.Millisecond})
	expectCode(t, request(h, "GET", "/v1/workflows", "", nil), http.StatusGatewayTimeout, "request_timeout")
	h = handler(t, brokenStore{Store: memory.New(), panicOnList: true}, Config{})
	response = request(h, "GET", "/v1/workflows", "", nil)
	expectCode(t, response, http.StatusInternalServerError, "internal_error")
	if strings.Contains(response.Body.String(), "private implementation") {
		t.Fatal("panic details leaked")
	}
}

func TestInvalidLookupsAndLimits(t *testing.T) {
	h := handler(t, memory.New(), Config{})
	for _, value := range []string{"0", "101", "-1", "abc", "", "1&limit=2"} {
		expectCode(t, request(h, "GET", "/v1/workflows?limit="+value, "", nil), http.StatusBadRequest, "invalid_limit")
	}
	expectCode(t, request(h, "GET", "/v1/workflows/invalid", "", nil), http.StatusBadRequest, "invalid_workflow_id")
	expectCode(t, request(h, "GET", "/v1/workflows/"+model.ID(), "", nil), http.StatusNotFound, "not_found")
	expectCode(t, request(h, "POST", "/v1/workflows/"+model.ID()+"/cancel", "", nil), http.StatusNotFound, "not_found")
	expectCode(t, request(h, "GET", "/missing", "", nil), http.StatusNotFound, "not_found")
	if response := request(h, "DELETE", "/v1/workflows", "", nil); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported method: %d", response.Code)
	}
}

func TestWorkerMetricsAreAuthenticatedWithoutWorkflowRoutes(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "forge_task_active 0\n") })
	h := handler(t, memory.New(), Config{Token: "test-secret", Metrics: metrics, MetricsOnly: true})
	expectCode(t, request(h, "GET", "/metrics", "", nil), http.StatusUnauthorized, "unauthorized")
	response := request(h, "GET", "/metrics", "", map[string]string{"Authorization": "Bearer test-secret"})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "forge_task_active 0") {
		t.Fatalf("metrics unavailable: %s", response.Body.String())
	}
	expectCode(t, request(h, "GET", "/v1/workflows", "", map[string]string{"Authorization": "Bearer test-secret"}), http.StatusNotFound, "not_found")
	if response := request(h, "GET", "/healthz", "", nil); response.Code != http.StatusOK {
		t.Fatal("metrics listener health unavailable")
	}
}
