// Package config parses command-line flags and environment variables without
// reading .env files or contacting external services.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const Usage = `Forge — agent execution engine

Usage: forge [-mode demo|api|worker|migrate] [options]

  -mode          demo (default), api, worker, or migrate
  -addr          HTTP listen address (default 127.0.0.1:8080)
  -concurrency   Task slots per worker (default 256)
  -demo-nodes    Embedded worker pools in demo mode (default 10)
  -worker-id     Unique worker ID (generated if omitted)
  -metrics-addr  Optional worker-only metrics listener (disabled by default)

Environment: FORGE_MODE, FORGE_ADDR, FORGE_CONCURRENCY, FORGE_DEMO_NODES,
FORGE_WORKER_ID, FORGE_API_TOKEN, DATABASE_URL, FORGE_METRICS_ADDR,
OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME, FORGE_TRACE_SAMPLE_RATIO,
FORGE_CLOUD_REGION.
Flags override their corresponding environment variables. Secrets are env-only.
Demo mode uses volatile memory and simulated agents; no cloud service is called.
`

type Config struct {
	Mode             string
	Addr             string
	Concurrency      int
	DemoNodes        int
	WorkerID         string
	Token            string
	DatabaseURL      string
	MetricsAddr      string
	TraceEndpoint    string
	TraceService     string
	TraceSampleRatio float64
	CloudRegion      string
}

func Load(args []string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("environment reader is required")
	}
	value := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	// Parse numeric values after flags so a valid flag can override a bad env.
	concurrency := value("FORGE_CONCURRENCY", "256")
	nodes := value("FORGE_DEMO_NODES", "10")
	cfg := Config{Token: getenv("FORGE_API_TOKEN"), DatabaseURL: getenv("DATABASE_URL"), TraceEndpoint: getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), TraceService: getenv("OTEL_SERVICE_NAME")}
	cfg.CloudRegion = value("FORGE_CLOUD_REGION", "us-east-1")
	flags := flag.NewFlagSet("forge", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.Mode, "mode", value("FORGE_MODE", "demo"), "execution mode")
	flags.StringVar(&cfg.Addr, "addr", value("FORGE_ADDR", "127.0.0.1:8080"), "HTTP listen address")
	flags.StringVar(&cfg.WorkerID, "worker-id", getenv("FORGE_WORKER_ID"), "unique worker ID")
	flags.StringVar(&cfg.MetricsAddr, "metrics-addr", getenv("FORGE_METRICS_ADDR"), "optional worker metrics address")
	flags.StringVar(&concurrency, "concurrency", concurrency, "slots per worker")
	flags.StringVar(&nodes, "demo-nodes", nodes, "embedded demo workers")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}
	var err error
	if cfg.Concurrency, err = strconv.Atoi(concurrency); err != nil {
		return cfg, errors.New("concurrency must be an integer")
	}
	if cfg.DemoNodes, err = strconv.Atoi(nodes); err != nil {
		return cfg, errors.New("demo-nodes must be an integer")
	}
	if cfg.TraceService == "" {
		cfg.TraceService = "forge-" + cfg.Mode
	}
	if cfg.TraceSampleRatio, err = strconv.ParseFloat(value("FORGE_TRACE_SAMPLE_RATIO", "0.1"), 64); err != nil {
		return cfg, errors.New("trace sample ratio must be a number from 0 to 1")
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	switch c.Mode {
	case "demo", "api", "worker", "migrate":
	default:
		return errors.New("mode must be demo, api, worker, or migrate")
	}
	if c.Concurrency < 1 || c.Concurrency > 4096 {
		return errors.New("concurrency must be 1–4096")
	}
	if c.DemoNodes < 1 || c.DemoNodes > 10 {
		return errors.New("demo-nodes must be 1–10")
	}
	if c.Mode == "demo" && c.Concurrency*c.DemoNodes > 4096 {
		return errors.New("demo mode supports at most 4096 total task slots")
	}
	if len(c.WorkerID) > 128 {
		return errors.New("worker ID must be at most 128 bytes")
	}
	if len(c.CloudRegion) > 64 {
		return errors.New("cloud region label must be at most 64 bytes")
	}
	if strings.ContainsAny(c.Token, " \t\r\n") {
		return errors.New("API token must not contain whitespace")
	}
	if math.IsNaN(c.TraceSampleRatio) || c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		return errors.New("trace sample ratio must be between 0 and 1")
	}
	if c.TraceEndpoint != "" {
		u, err := url.Parse(c.TraceEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("OTLP endpoint must be an HTTP(S) base URL without credentials, query, or fragment")
		}
	}
	if c.Mode == "demo" || c.Mode == "api" {
		if err := validateBind(c.Addr, c.Token); err != nil {
			return err
		}
	}
	if c.Mode == "worker" && c.MetricsAddr != "" {
		if err := validateBind(c.MetricsAddr, c.Token); err != nil {
			return err
		}
	}
	if c.Mode != "demo" {
		if c.DatabaseURL == "" {
			return fmt.Errorf("DATABASE_URL is required in %s mode", c.Mode)
		}
		u, err := url.Parse(c.DatabaseURL)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" {
			return errors.New("DATABASE_URL must be a postgres:// or postgresql:// connection URL with a host")
		}
	}
	return nil
}

func validateBind(address, token string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("HTTP address must use host:port syntax")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return errors.New("HTTP port must be 0–65535")
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !loopback && len(token) < 32 {
		return errors.New("listening beyond loopback requires an API token of at least 32 bytes")
	}
	return nil
}
