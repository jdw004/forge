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
	runtime, err := app.New(cfg, tasks, logger)
	if err != nil {
		return err
	}
	var listener net.Listener
	if cfg.Mode != "worker" {
		listener, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}
	return runtime.Run(ctx, listener)
}
