package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"lgllm/internal/auth"
	"lgllm/internal/config"
)

// TestServeAndShutdown exercises the real listener: bind an ephemeral port,
// answer a request and drain cleanly.
func TestServeAndShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Auth.Mode = "off"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(Options{
		Config: func() *config.Config { return &cfg },
		Log:    logger,
		Auth:   auth.New(nil, true),
	})

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	// Wait for the kernel-assigned port.
	deadline := time.Now().Add(3 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("server did not start in time")
		}
		time.Sleep(5 * time.Millisecond)
	}

	base := "http://" + srv.Addr()
	res, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}
