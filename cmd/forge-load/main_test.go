package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"testing"

	"forge/internal/loadtest"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	emptyEnv := func(string) string { return "" }
	err := run(context.Background(), []string{"-tasks", "1", "-workers", "1", "-concurrency", "1", "-task-duration", "1ms"}, emptyEnv, &out, io.Discard)
	var report loadtest.Report
	if err != nil || json.Unmarshal(out.Bytes(), &report) != nil || !report.Passed {
		t.Fatalf("output=%s, err=%v", out.String(), err)
	}
	for _, args := range [][]string{{"-backend", "invalid"}, {"-tasks", "0"}, {"extra"}} {
		out.Reset()
		if err := run(context.Background(), args, emptyEnv, &out, io.Discard); err == nil || out.Len() != 0 {
			t.Fatalf("args=%v output=%s err=%v", args, out.String(), err)
		}
	}
	if err := run(context.Background(), []string{"-help"}, emptyEnv, io.Discard, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
}

func TestRunFailureProducesJSON(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-backend", "http", "-url", "invalid"}, func(string) string { return "" }, &out, io.Discard)
	var report loadtest.Report
	if err == nil || json.Unmarshal(out.Bytes(), &report) != nil || report.Passed || report.Error == "" {
		t.Fatalf("output=%s, err=%v", out.String(), err)
	}
}
