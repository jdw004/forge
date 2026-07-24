package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/config"
	"forge/internal/memory"
	"forge/internal/model"
	"forge/internal/queue"
)

// A net.Pipe listener exercises the real HTTP server without opening sockets.
type pipeListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}
type pipeAddress struct{}

func (pipeAddress) Network() string    { return "pipe" }
func (pipeAddress) String() string     { return "local-test" }
func (l *pipeListener) Addr() net.Addr { return pipeAddress{} }
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	case <-l.done:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	case l.connections <- server:
		return client, nil
	}
}

func configuration(t *testing.T, mode string) config.Config {
	t.Helper()
	cfg, err := config.Load([]string{"-mode", mode, "-demo-nodes", "2", "-concurrency", "2"}, func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://unused@localhost/unused"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func start(t *testing.T, mode string, store queue.Store) (*http.Client, func()) {
	t.Helper()
	return startConfigured(t, configuration(t, mode), store)
}

func startConfigured(t *testing.T, cfg config.Config, store queue.Store) (*http.Client, func()) {
	t.Helper()
	a, err := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var listener net.Listener
	var client *http.Client
	if cfg.Mode != "worker" || cfg.MetricsAddr != "" {
		pipe := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
		listener = pipe
		transport := &http.Transport{DialContext: pipe.dial, DisableKeepAlives: true}
		client = &http.Client{Transport: transport, Timeout: 2 * time.Second}
		t.Cleanup(transport.CloseIdleConnections)
	}
	finished := make(chan error, 1)
	go func() { finished <- a.Run(ctx, listener) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-finished:
				if err != nil {
					t.Errorf("application failed: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("application did not shut down")
			}
		})
	}
	t.Cleanup(stop)
	return client, stop
}

func eventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func submit(t *testing.T, client *http.Client, body string) model.Workflow {
	t.Helper()
	response, err := client.Post("http://forge/v1/workflows", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("submission returned %d", response.StatusCode)
	}
	var workflow model.Workflow
	if err := json.NewDecoder(response.Body).Decode(&workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

const workflowJSON = `{"name":"local","tasks":[{"id":"research","steps":2,"step_ms":5,"tokens_per_step":2,"token_limit":10,"runtime_ms":1000,"max_attempts":2},{"id":"summarize","depends_on":["research"],"steps":2,"step_ms":5,"tokens_per_step":2,"token_limit":10,"runtime_ms":1000,"max_attempts":2}]}`

func TestDemoServesHTTPAndExecutesWorkflow(t *testing.T) {
	store := memory.New()
	client, stop := start(t, "demo", store)
	eventually(t, "two demo worker registrations", func() bool {
		nodes, err := store.Nodes(context.Background())
		return err == nil && len(nodes) == 2
	})
	created := submit(t, client, workflowJSON)
	eventually(t, "workflow completion through the HTTP API", func() bool {
		response, err := client.Get("http://forge/v1/workflows/" + created.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var got model.Workflow
		if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.State == model.Succeeded
	})
	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range got.Tasks {
		if task.Attempt != 1 || task.Tokens != 4 {
			t.Fatalf("incorrect execution: %+v", task)
		}
	}
	eventually(t, "committed task metrics", func() bool {
		response, err := client.Get("http://forge/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode == http.StatusOK && strings.Contains(string(body), `forge_tasks_completed_total{state="succeeded"} 2`) && strings.Contains(string(body), "forge_worker_capacity 4")
	})
	stop()
	nodes, _ := store.Nodes(context.Background())
	if len(nodes) != 0 {
		t.Fatal("workers survived application shutdown")
	}
}

func TestAPIOnlyDoesNotRunEmbeddedWorkers(t *testing.T) {
	store := memory.New()
	client, stop := start(t, "api", store)
	created := submit(t, client, workflowJSON)
	stop()
	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range got.Tasks {
		if task.State != model.Pending || task.Attempt != 0 {
			t.Fatal("API-only mode executed tasks")
		}
	}
}

func TestWorkerOnlyExecutesAndJoinsInterruptedAgents(t *testing.T) {
	store := memory.New()
	_, stop := start(t, "worker", store)
	task := model.TaskSpec{ID: "long", Steps: 10, StepMS: 1000, TokensPerStep: 1, TokenLimit: 100, RuntimeMS: 30000, MaxAttempts: 2}
	w, _, err := store.Create(context.Background(), model.Spec{Name: "shutdown", Tasks: []model.TaskSpec{task}}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "active execution", func() bool {
		got, _ := store.Get(context.Background(), w.ID)
		return got.Tasks[0].State == model.Running
	})
	stop()
	got, _ := store.Get(context.Background(), w.ID)
	if got.Tasks[0].State != model.Running {
		t.Fatal("interrupted claim should remain recoverable by lease expiry")
	}
	nodes, _ := store.Nodes(context.Background())
	if len(nodes) != 0 {
		t.Fatal("worker did not retire")
	}
}

type failingRegistration struct{ queue.Store }

func (f failingRegistration) Heartbeat(context.Context, model.Node) error {
	return errors.New("registration unavailable")
}

func TestWorkerStartupFailureStopsApplication(t *testing.T) {
	a, err := New(configuration(t, "demo"), failingRegistration{memory.New()}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = a.Run(ctx, l)
	if err == nil || !strings.Contains(err.Error(), "registration unavailable") {
		t.Fatalf("lost startup error: %v", err)
	}
	select {
	case <-l.done:
	default:
		t.Fatal("HTTP listener survived worker startup failure")
	}
}

func TestListenerFailureStopsWorkers(t *testing.T) {
	store := memory.New()
	a, err := New(configuration(t, "demo"), store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Run(ctx, l); err == nil {
		t.Fatal("listener failure was ignored")
	}
	nodes, _ := store.Nodes(context.Background())
	if len(nodes) != 0 {
		t.Fatal("workers survived listener failure")
	}
}

func TestOptionalWorkerListenerExposesOnlyMetricsAndHealth(t *testing.T) {
	cfg := configuration(t, "worker")
	cfg.MetricsAddr = "127.0.0.1:9091"
	client, stop := startConfigured(t, cfg, memory.New())
	for _, test := range []struct {
		path   string
		status int
	}{
		{"/metrics", http.StatusOK}, {"/healthz", http.StatusOK}, {"/readyz", http.StatusOK}, {"/v1/workflows", http.StatusNotFound},
	} {
		response, err := client.Get("http://forge" + test.path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("%s: got %d, want %d", test.path, response.StatusCode, test.status)
		}
	}
	stop()
}
