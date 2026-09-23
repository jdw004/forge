package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOpenDoesNotExposeConnectionSecrets(t *testing.T) {
	for _, dsn := range []string{
		"postgres://forge:private-password@db.example/forge?pool_max_conns=invalid",
		"postgres://forge:private-password@db.example:invalid/forge",
		"host=db.example password=private-password connect_timeout=invalid",
	} {
		s, err := Open(context.Background(), dsn)
		if s != nil || err == nil {
			t.Fatalf("accepted invalid settings: store=%v error=%v", s, err)
		}
		if strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), dsn) || errors.Unwrap(err) != nil {
			t.Fatalf("connection details exposed: %v", err)
		}
	}
}

func TestOpenPreservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := Open(ctx, "postgres://forge:private-password@127.0.0.1:1/forge?sslmode=disable")
	if s != nil || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private-password") {
		t.Fatalf("store=%v error=%v", s, err)
	}
}
