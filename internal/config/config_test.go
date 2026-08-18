package config

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestDefaultsAndFlagPrecedence(t *testing.T) {
	cfg, err := Load(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "demo" || cfg.Addr != "127.0.0.1:8080" || cfg.DemoNodes != 10 || cfg.Concurrency != 256 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	cfg, err = Load([]string{"-mode", "demo", "-concurrency", "4", "-demo-nodes", "2"}, env(map[string]string{"FORGE_MODE": "worker", "FORGE_CONCURRENCY": "invalid"}))
	if err != nil || cfg.Mode != "demo" || cfg.Concurrency != 4 || cfg.DemoNodes != 2 {
		t.Fatalf("flags did not override env: %+v %v", cfg, err)
	}
	if _, err := Load([]string{"-h"}, env(nil)); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
}

func TestConfigurationRejectsUnsafeOrInvalidSettings(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		values map[string]string
	}{
		{"unknown mode", []string{"-mode", "cloud"}, nil},
		{"missing database", []string{"-mode", "api"}, nil},
		{"bad database", []string{"-mode", "worker"}, map[string]string{"DATABASE_URL": "https://db.example/forge"}},
		{"remote without auth", []string{"-addr", "0.0.0.0:8080"}, nil},
		{"remote weak token", []string{"-addr", ":8080"}, map[string]string{"FORGE_API_TOKEN": "short"}},
		{"IPv6 remote", []string{"-addr", "[::]:8080"}, nil},
		{"bad port", []string{"-addr", "127.0.0.1:99999"}, nil},
		{"bad address", []string{"-addr", "127.0.0.1"}, nil},
		{"zero slots", []string{"-concurrency", "0"}, nil},
		{"zero nodes", []string{"-demo-nodes", "0"}, nil},
		{"excess capacity", []string{"-concurrency", "4096", "-demo-nodes", "2"}, nil},
		{"bad token", nil, map[string]string{"FORGE_API_TOKEN": "with whitespace"}},
		{"unexpected argument", []string{"extra"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(test.args, env(test.values)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestLoopbackAndAuthenticatedRemoteBinding(t *testing.T) {
	for _, address := range []string{"localhost:8080", "127.0.0.1:0", "[::1]:8080"} {
		if _, err := Load([]string{"-addr", address}, env(nil)); err != nil {
			t.Fatalf("loopback rejected: %v", err)
		}
	}
	for _, mode := range []string{"api", "worker", "migrate"} {
		_, err := Load([]string{"-mode", mode, "-addr", "0.0.0.0:8080"}, env(map[string]string{
			"FORGE_API_TOKEN": strings.Repeat("x", 32), "DATABASE_URL": "postgres://forge:password@localhost/forge?sslmode=disable",
		}))
		if err != nil {
			t.Fatalf("valid %s mode rejected: %v", mode, err)
		}
	}
}

func TestTelemetryConfiguration(t *testing.T) {
	for _, values := range []map[string]string{
		{"FORGE_TRACE_SAMPLE_RATIO": "NaN"}, {"FORGE_TRACE_SAMPLE_RATIO": "1.5"}, {"FORGE_TRACE_SAMPLE_RATIO": "bad"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:secret@collector"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector?token=secret"},
		{"FORGE_MODE": "worker", "DATABASE_URL": "postgres://localhost/forge", "FORGE_METRICS_ADDR": "0.0.0.0:9091"},
	} {
		if _, err := Load(nil, env(values)); err == nil {
			t.Fatal("invalid telemetry configuration accepted")
		}
	}
	cfg, err := Load(nil, env(map[string]string{"FORGE_MODE": "worker", "DATABASE_URL": "postgres://localhost/forge", "FORGE_METRICS_ADDR": "127.0.0.1:9091", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318", "FORGE_TRACE_SAMPLE_RATIO": "0.5"}))
	if err != nil || cfg.TraceSampleRatio != .5 || cfg.MetricsAddr != "127.0.0.1:9091" || cfg.TraceService != "forge-worker" {
		t.Fatalf("telemetry configuration: %+v %v", cfg, err)
	}
}

func TestMountedSecretsConfigureWorker(t *testing.T) {
	directory := t.TempDir()
	token := strings.Repeat("a", 64)
	database := "postgres://forge:private-password@postgres:5432/forge?sslmode=disable&pool_max_conns=8"
	for name, value := range map[string]string{"token": token, "database": database} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load([]string{"-mode", "worker", "-metrics-addr", "0.0.0.0:9091"}, env(map[string]string{
		"FORGE_API_TOKEN_FILE": filepath.Join(directory, "token"), "DATABASE_URL_FILE": filepath.Join(directory, "database"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != token || cfg.DatabaseURL != database {
		t.Fatal("mounted secret values were not loaded correctly")
	}
}

func TestMountedSecretErrorsDoNotExposeValues(t *testing.T) {
	directory := t.TempDir()
	for name, value := range map[string]string{"empty": "\n", "large": strings.Repeat("x", 8193), "token": "private-secret\n"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, values := range []map[string]string{
		{"FORGE_API_TOKEN_FILE": filepath.Join(directory, "missing")},
		{"FORGE_API_TOKEN_FILE": filepath.Join(directory, "empty")},
		{"FORGE_API_TOKEN_FILE": filepath.Join(directory, "large")},
		{"FORGE_API_TOKEN": "private-secret", "FORGE_API_TOKEN_FILE": filepath.Join(directory, "token")},
		{"DATABASE_URL": "postgres://private-secret@localhost/forge", "DATABASE_URL_FILE": filepath.Join(directory, "token")},
	} {
		_, err := Load(nil, env(values))
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("expected sanitized configuration error; got %v", err)
		}
	}
	if _, err := Load([]string{"-h"}, env(map[string]string{"FORGE_API_TOKEN_FILE": "/missing"})); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help should not read secrets: %v", err)
	}
}
