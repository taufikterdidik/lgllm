package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lgllm/internal/auth"
	"lgllm/internal/config"
	"lgllm/internal/registry"
	"lgllm/internal/upstream"
)

func testConfig(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	cfg := config.Default()
	// Tests that do not exercise auth run with it off; httptest clients are not
	// loopback and would otherwise be rejected by the localhost policy.
	cfg.Auth.Mode = "off"
	// One usable provider, so /readyz and /v1/models have something to report.
	// Proxy tests point its base_url at an httptest server.
	cfg.Providers = []config.Provider{{
		ID:      "testprov",
		BaseURL: "https://upstream.invalid/v1",
		Format:  "openai",
		Auth:    "bearer",
		Models:  []config.Model{{ID: "test-model"}},
	}}
	cfg.Connections = []config.Connection{{ID: "test-conn", Provider: "testprov", APIKey: "sk-upstream"}}
	cfg.DefaultProvider = "testprov"
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return &cfg
}

func newTestServer(t *testing.T, cfg *config.Config, keys []string) *Server {
	t.Helper()
	if keys == nil {
		keys = cfg.Auth.Keys
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := mustRegistry(t, cfg)
	return New(Options{
		Config:   func() *config.Config { return cfg },
		Log:      logger,
		Auth:     auth.New(keys, cfg.Auth.Mode == "off"),
		Registry: func() *registry.Registry { return reg },
		Upstream: upstream.New(cfg.Upstream),
	})
}

func do(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHealthzAndVersionArePublic(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Auth.Mode = "required"
		c.Auth.Keys = []string{"sk-0123456789abcdef"}
	})
	s := newTestServer(t, cfg, nil)

	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := do(s, req)
		if res.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, res.Code)
		}
		if ct := res.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s content-type = %q", path, ct)
		}
		if res.Header().Get(requestIDHeader) == "" {
			t.Errorf("GET %s missing %s header", path, requestIDHeader)
		}
	}
}

func TestAuthRequiredRejectsMissingKey(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Auth.Mode = "required"
		c.Auth.Keys = []string{"sk-0123456789abcdef"}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
	if got := res.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	var body errorEnvelope
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Error.Type != "invalid_api_key" {
		t.Errorf("error.type = %q", body.Error.Type)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-0123456789abcdef")
	res = do(s, req)
	if res.Code != http.StatusOK {
		t.Fatalf("with a valid key status = %d, want 200", res.Code)
	}
}

func TestAuthLocalhostTrustsLoopback(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Auth.Mode = "localhost"
		c.Auth.Keys = []string{"sk-0123456789abcdef"}
	})
	s := newTestServer(t, cfg, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	if res := do(s, req); res.Code == http.StatusUnauthorized {
		t.Fatal("loopback client must not require a key in localhost mode")
	}

	remote := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	remote.RemoteAddr = "203.0.113.9:5555"
	if res := do(s, remote); res.Code != http.StatusUnauthorized {
		t.Fatalf("public client status = %d, want 401", res.Code)
	}
}

func TestAuthOffSkipsVerification(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Auth.Mode = "off"
		c.Server.Listen = "127.0.0.1:20128"
	})
	s := newTestServer(t, cfg, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	if res := do(s, req); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
}

func TestPreflightIsAnsweredWithoutAuth(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Auth.Mode = "required"
		c.Auth.Keys = []string{"sk-0123456789abcdef"}
	})
	s := newTestServer(t, cfg, nil)

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	res := do(s, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", res.Code)
	}
	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Allow-Origin = %q", got)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
}

func TestRootListsEndpoints(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "lgllm" {
		t.Errorf("name = %v", body["name"])
	}
}

func TestRequestIDIsPropagated(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(requestIDHeader, "caller-supplied-id")
	res := do(s, req)
	if got := res.Header().Get(requestIDHeader); got != "caller-supplied-id" {
		t.Errorf("request id = %q, want the caller's value", got)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req2.Header.Set(requestIDHeader, strings.Repeat("x", 100))
	res2 := do(s, req2)
	if got := res2.Header().Get(requestIDHeader); len(got) >= 100 {
		t.Errorf("over-long request id should be replaced, got %d chars", len(got))
	}
}

func TestPanicBecomes500(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	prevDefault := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	cfg := config.Default()
	s := New(Options{
		Config:   func() *config.Config { return &cfg },
		Log:      logger,
		Auth:     auth.New(nil, true),
		Registry: func() *registry.Registry { return mustRegistry(t, &cfg) },
		Upstream: upstream.New(cfg.Upstream),
	})

	h := s.withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Server.MaxBodyBytes = 1024
	})
	s := newTestServer(t, cfg, nil)

	big := strings.Repeat("a", 4096)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	if res := do(s, req); res.Code == http.StatusOK {
		t.Fatal("oversized body must not be accepted")
	}
}

func TestStatusRecorderPreservesFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec}
	var _ http.Flusher = sr // compile-time check: SSE needs this in phase 2

	sr.WriteHeader(http.StatusTeapot)
	sr.WriteHeader(http.StatusOK)
	if sr.status != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 from the first WriteHeader", sr.status)
	}
}

// mustRegistry compiles a config the test expects to be valid.
func mustRegistry(t *testing.T, cfg *config.Config) *registry.Registry {
	t.Helper()
	reg, err := registry.New(cfg)
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	return reg
}
