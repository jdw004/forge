package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"forge/internal/app"
	"forge/internal/config"
	"forge/internal/memory"
	"forge/internal/queue"
	"forge/internal/store"
	"forge/internal/telemetry"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, logger)
	stop()
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(config.Usage)
		return
	}
	if err != nil {
		logger.Error("Forge stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, logger *slog.Logger) error {
	cfg, err := config.Load(args, getenv)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	var tasks queue.Store
	if cfg.Mode == "demo" {
		tasks = memory.New()
	} else {
		connect, cancel := context.WithTimeout(ctx, 10*time.Second)
		database, err := store.Open(connect, cfg.DatabaseURL)
		cancel()
		if err != nil {
			return err
		}
		defer database.Close()
		tasks = database
		if cfg.Mode == "migrate" {
			migration, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			if err := database.Migrate(migration); err != nil {
				return err
			}
			logger.Info("Database migrations applied")
			return nil
		}
	}
	monitor, err := telemetry.New(ctx, telemetry.Config{ServiceName: cfg.TraceService, Endpoint: cfg.TraceEndpoint, SampleRatio: cfg.TraceSampleRatio})
	if err != nil {
		return fmt.Errorf("initialize telemetry: %w", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := monitor.Shutdown(shutdown); err != nil {
			logger.Warn("trace export shutdown failed", "error", err)
		}
	}()
	runtime, err := app.New(cfg, tasks, logger, monitor)
	if err != nil {
		return err
	}
	var listener net.Listener
	address := cfg.Addr
	if cfg.Mode == "worker" {
		address = cfg.MetricsAddr
	}
	if address != "" {
		listener, err = (&net.ListenConfig{}).Listen(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}
	return runtime.Run(ctx, listener)
}
