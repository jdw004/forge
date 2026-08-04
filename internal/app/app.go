// Package app connects the HTTP API and workers while keeping process startup
// and PostgreSQL provisioning in the command package.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"forge/internal/api"
	"forge/internal/config"
	"forge/internal/console"
	"forge/internal/queue"
	"forge/internal/telemetry"
	"forge/internal/worker"
)

type App struct {
	cfg     config.Config
	logger  *slog.Logger
	handler http.Handler
	workers []*worker.Worker
	running atomic.Bool
}

func New(cfg config.Config, store queue.Store, logger *slog.Logger, monitor *telemetry.Telemetry) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Mode == "migrate" {
		return nil, errors.New("migration mode does not run an application")
	}
	if store == nil {
		return nil, errors.New("application requires a store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if monitor == nil {
		monitor = telemetry.Local()
	}
	a := &App{cfg: cfg, logger: logger}
	if cfg.Mode != "worker" || cfg.MetricsAddr != "" {
		handler, err := api.New(store, api.Config{
			Token: cfg.Token, Logger: logger, Metrics: monitor.Handler(), MetricsOnly: cfg.Mode == "worker", Middleware: monitor.HTTPMiddleware,
			Console: console.Handler(), Mode: cfg.Mode, CloudRegion: cfg.CloudRegion, TracingConfigured: cfg.TraceEndpoint != "",
		})
		if err != nil {
			return nil, err
		}
		a.handler = handler
	}
	count := 0
	if cfg.Mode == "demo" {
		count = cfg.DemoNodes
	}
	if cfg.Mode == "worker" {
		count = 1
	}
	for i := 0; i < count; i++ {
		id := cfg.WorkerID
		if cfg.Mode == "demo" {
			id = fmt.Sprintf("demo-%02d", i+1)
		}
		w, err := worker.New(store, worker.Config{ID: id, Concurrency: cfg.Concurrency, Logger: logger, Observer: monitor})
		if err != nil {
			return nil, err
		}
		a.workers = append(a.workers, w)
	}
	monitor.SetCapacity(len(a.workers) * cfg.Concurrency)
	return a, nil
}

// Run takes ownership of the listener, serves until cancellation, and joins all
// workers before returning. Worker mode takes a nil listener unless a metrics
// address is configured; that listener exposes only metrics and health probes.
// The caller owns the store and must close it only after Run returns.
func (a *App) Run(parent context.Context, listener net.Listener) error {
	if a.handler != nil && listener == nil {
		return errors.New("HTTP mode requires a listener")
	}
	if a.handler == nil && listener != nil {
		return errors.New("no HTTP listener is configured")
	}
	if !a.running.CompareAndSwap(false, true) {
		return errors.New("application is already running")
	}
	defer a.running.Store(false)
	if listener != nil {
		defer listener.Close()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if ctx.Err() != nil {
		return nil
	}

	var runError error
	var firstError sync.Once
	fail := func(err error) {
		firstError.Do(func() { runError = err; cancel() })
	}
	var wg sync.WaitGroup
	for _, w := range a.workers {
		wg.Add(1)
		go func(w *worker.Worker) {
			defer wg.Done()
			if err := w.Run(ctx); err != nil && ctx.Err() == nil {
				fail(fmt.Errorf("worker %s: %w", w.ID(), err))
			}
		}(w)
	}

	var server *http.Server
	if a.handler != nil {
		server = &http.Server{
			Handler: a.handler, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
			IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
			ErrorLog: slog.NewLogLogger(a.logger.Handler(), slog.LevelError),
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fail(fmt.Errorf("HTTP server: %w", err))
			}
		}()
		a.logger.Info("Forge listening", "mode", a.cfg.Mode, "address", listener.Addr().String(), "embedded_workers", len(a.workers))
	} else {
		a.logger.Info("Forge worker starting", "worker", a.workers[0].ID(), "capacity", a.cfg.Concurrency)
	}
	if a.cfg.Mode == "demo" {
		a.logger.Info("Local simulation active", "storage", "memory", "cloud_connected", false)
	}
	<-ctx.Done()
	if server != nil {
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		err := server.Shutdown(shutdown)
		stop()
		if err != nil {
			_ = server.Close()
			fail(fmt.Errorf("HTTP shutdown: %w", err))
		}
	}
	wg.Wait()
	return runError
}
