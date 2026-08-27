package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"forge/internal/config"
	"forge/internal/loadtest"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	cancel()
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out, diagnostics io.Writer) error {
	options := loadtest.Defaults()
	flags := flag.NewFlagSet("forge-load", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	backend := flags.String("backend", "memory", "memory or http; memory uses one process")
	baseURL := flags.String("url", "http://127.0.0.1:8080", "existing API URL for HTTP mode")
	flags.IntVar(&options.Tasks, "tasks", options.Tasks, "tasks to submit (1–5000)")
	flags.IntVar(&options.Workers, "workers", options.Workers, "memory worker pools (HTTP mode observes the existing deployment)")
	flags.IntVar(&options.Concurrency, "concurrency", options.Concurrency, "slots per memory worker")
	flags.StringVar(&options.Scenario, "scenario", options.Scenario, "batch, token, or runtime")
	flags.DurationVar(&options.TaskDuration, "task-duration", options.TaskDuration, "nominal batch task duration")
	flags.DurationVar(&options.RuntimeLimit, "runtime-limit", options.RuntimeLimit, "runtime breaker budget")
	flags.DurationVar(&options.Timeout, "timeout", options.Timeout, "overall run deadline")
	flags.DurationVar(&options.PollInterval, "poll", options.PollInterval, "progress poll interval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := options.Validate(); err != nil {
		return err
	}
	var report loadtest.Report
	var err error
	switch *backend {
	case "memory":
		report, err = loadtest.RunMemory(ctx, options)
	case "http":
		token, secretError := config.ReadSecret(getenv, "FORGE_API_TOKEN")
		if secretError != nil {
			return secretError
		}
		report, err = loadtest.RunHTTP(ctx, options, *baseURL, token, nil)
	default:
		return errors.New("backend must be memory or http")
	}
	if err != nil {
		report.Error = err.Error()
		report.Passed = false
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if writeError := encoder.Encode(report); writeError != nil {
		return writeError
	}
	return err
}
