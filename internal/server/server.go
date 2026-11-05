// Package server owns the HTTP listener, the middleware chain and routing.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"lgllm/internal/auth"
	"lgllm/internal/config"
	"lgllm/internal/registry"
	"lgllm/internal/upstream"
)

// Server wraps http.Server with lgllm's routing and lifecycle.
type Server struct {
	// Config is a getter rather than a value so a live reload is visible to
	// request handling without rebuilding the server.
	Config func() *config.Config
	Log    *slog.Logger
	Auth   *auth.Verifier

	// Registry returns the provider index for the current config. It is a getter
	// for the same reason as Config: a reload swaps it, and in-flight requests
	// keep the snapshot they started with.
	Registry func() *registry.Registry
	// Upstream is the shared outbound client. Its transport is reloaded in place.
	Upstream *upstream.Client

	mux *http.ServeMux
	srv *http.Server

	mu      sync.Mutex
	addr    string
	started time.Time
}

// Options are the dependencies needed to construct a Server.
type Options struct {
	Config   func() *config.Config
	Log      *slog.Logger
	Auth     *auth.Verifier
	Registry func() *registry.Registry
	Upstream *upstream.Client
}

// New builds the server and registers routes. The listener is not opened yet.
func New(opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Config == nil {
		// Middleware reads the config on every request, so a nil getter would
		// panic. Defaults are a valid configuration, captured once.
		def := config.Default()
		opts.Config = func() *config.Config { return &def }
	}
	if opts.Auth == nil {
		// Fail closed: an empty verifier rejects every key.
		opts.Auth = auth.New(nil, false)
	}
	if opts.Registry == nil {
		// A nil getter would panic on the first /v1 request. Compile the
		// current config once; if it is not routable, an empty index answers
		// "no providers configured" and /readyz reports degraded instead.
		reg, err := registry.New(opts.Config())
		if err != nil {
			reg, _ = registry.New(&config.Config{})
		}
		opts.Registry = func() *registry.Registry { return reg }
	}
	if opts.Upstream == nil {
		opts.Upstream = upstream.New(config.Default().Upstream)
	}
	s := &Server{
		Config:   opts.Config,
		Log:      opts.Log,
		Auth:     opts.Auth,
		Registry: opts.Registry,
		Upstream: opts.Upstream,
		mux:      http.NewServeMux(),
		started:  time.Now(),
	}
	s.routes()
	return s
}

// Handler returns the full middleware chain. Exposed for tests and for callers
// that want to mount lgllm inside their own listener.
func (s *Server) Handler() http.Handler {
	// Middleware reads s.Config() per request rather than capturing values, so a
	// reload changes limits and CORS policy without rebuilding the handler.

	// Order is deliberate:
	//   recover   — outermost, so a panic in any layer becomes a 500
	//   requestID — everything below can correlate its log lines
	//   accessLog — records the final status, including auth failures
	//   CORS      — must answer preflight before auth can reject it
	//   auth      — cheap rejection before any body work happens
	//   bodyLimit — bounds only the handlers that read a body
	var h http.Handler = s.mux
	h = s.withBodyLimit(h)
	h = s.withAuth(h)
	h = s.withCORS(h)
	h = s.withAccessLog(h)
	h = s.withRequestID(h)
	h = s.withRecover(h)
	return h
}

// ListenAndServe binds and serves until Shutdown is called.
func (s *Server) ListenAndServe() error {
	cfg := s.Config()
	// The listener is bound from the config at startup; a reload that changes
	// the address is reported by main and needs a restart.
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		ErrorLog:          slog.NewLogLogger(s.Log.Handler(), slog.LevelWarn),
	}

	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", srv.Addr, err)
	}
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()

	s.Log.Info("http server listening", "addr", s.Addr())

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// Addr reports the bound address. With "listen: 127.0.0.1:0" this is the
// kernel-assigned port, which tests rely on.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown drains connections within the configured timeout, then closes.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}

	timeout := s.Config().Server.ShutdownTimeout.Std()
	drainCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s.Log.Info("http server shutting down", "timeout", timeout.String())
	if err := srv.Shutdown(drainCtx); err != nil {
		// A stream that refuses to end must not block process exit forever.
		s.Log.Warn("graceful shutdown timed out; closing connections", "err", err)
		return srv.Close()
	}
	return nil
}
