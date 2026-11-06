package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"lgllm/internal/auth"
)

// requestIDHeader is echoed to the client so a user report can be matched to
// exactly one log line.
const requestIDHeader = "X-Request-Id"

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxLogger
)

// LoggerFrom returns the request-scoped logger, or the default logger.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// RequestID returns the request identifier, or "" outside a request.
func RequestID(ctx context.Context) string {
	if id, ok := ctx.Value(ctxRequestID).(string); ok {
		return id
	}
	return ""
}

// withRequestID assigns (or adopts) a request id and a request-scoped logger.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if len(id) == 0 || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)

		ctx := context.WithValue(r.Context(), ctxRequestID, id)
		ctx = context.WithValue(ctx, ctxLogger, s.Log.With("req_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "t" + time.Now().Format("20060102150405.000000")
	}
	return hex.EncodeToString(b[:])
}

// withRecover turns a handler panic into a 500 instead of dropping the process
// and every other in-flight stream with it.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				// Deliberate abort; net/http handles this one itself.
				panic(rec)
			}
			LoggerFrom(r.Context()).Error("panic while serving request",
				"method", r.Method,
				"path", r.URL.Path,
				"panic", rec,
				"stack", string(debug.Stack()),
			)
			if rec2, ok := w.(*statusRecorder); !ok || !rec2.sent {
				writeJSONError(w, http.StatusInternalServerError, "internal server error", "internal_error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures status and byte count for the access log while
// preserving Flusher and Unwrap (both needed once SSE arrives in phase 2).
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	sent    bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.sent {
		return
	}
	w.status = code
	w.sent = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.sent {
		w.status = http.StatusOK
		w.sent = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withAccessLog emits one line per request, after the handler returns (so a
// stream is logged when its last byte is written).
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		log := LoggerFrom(r.Context())
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"dur_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
		}
		switch {
		case rec.status >= 500:
			log.Error("request", attrs...)
		case rec.status >= 400:
			log.Warn("request", attrs...)
		default:
			log.Info("request", attrs...)
		}
	})
}

// withAuth rejects unauthenticated calls before any handler work happens.
//
// In "localhost" mode loopback clients are trusted so a locally installed CLI
// works without configuration; anything else must present a key. This matches
// how these proxies are actually run on a developer machine.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A preflight never carries credentials and must be answered.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// The management API has its own credential, handled by withAdminAuth.
		if isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		cfg := s.Config()
		switch cfg.Auth.Mode {
		case "off":
			next.ServeHTTP(w, r)
			return
		case "localhost":
			if isLoopbackRemote(r) {
				next.ServeHTTP(w, r)
				return
			}
		}

		token := auth.ExtractToken(r)
		if !s.Auth.Verify(token) {
			log := LoggerFrom(r.Context())
			if token == "" {
				log.Warn("rejected request: missing api key",
					"path", r.URL.Path, "remote", clientIP(r))
			} else {
				log.Warn("rejected request: invalid api key",
					"path", r.URL.Path, "key", auth.Mask(token), "remote", clientIP(r))
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="lgllm"`)
			if r.URL.Path == "/v1/messages" {
				writeAnthropicError(w, http.StatusUnauthorized,
					"invalid or missing API key", "authentication_error")
			} else {
				writeJSONError(w, http.StatusUnauthorized, "invalid or missing API key", "invalid_api_key")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isPublicPath lists endpoints that must answer without credentials:
// liveness, readiness and the version banner.
func isPublicPath(p string) bool {
	switch p {
	case "/healthz", "/readyz", "/version":
		return true
	}
	// The management UI is static and carries no credentials; its API calls are
	// still gated by withAdminAuth.
	return p == "/ui" || strings.HasPrefix(p, "/ui/")
}

// isLoopbackRemote reports whether the transport peer is on the loopback
// interface.
func isLoopbackRemote(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return isLoopbackHost(host)
}

// isLoopbackHost accepts the textual forms net/http can produce.
func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clientIP is the transport peer address. X-Forwarded-For is deliberately
// ignored: no reverse proxy is assumed, and trusting it would let any client
// spoof its way into the trusted localhost branch of withAuth.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// withBodyLimit caps request bodies so an oversized upload cannot exhaust
// memory before a handler has a chance to reject it.
func (s *Server) withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limit := s.Config().Server.MaxBodyBytes; r.Body != nil && limit > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// withCORS answers preflights and decorates responses for browser clients.
//
// The proxy is POST-only and uses no cookies, so a permissive origin with an
// explicit header allow-list is safe; credentials are still enforced by
// withAuth for non-loopback callers.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins := s.Config().Server.CORSOrigins
		if origin := r.Header.Get("Origin"); origin != "" {
			if originAllowed(origin, origins) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
		}

		if r.Method == http.MethodOptions {
			if r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers",
					"Authorization, Content-Type, X-Api-Key, Anthropic-Version, Anthropic-Beta, X-Lgllm-Token-Saver")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(origin string, allow []string) bool {
	for _, a := range allow {
		if a == "*" || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

// withRateLimit enforces auth.rate_limit after authentication.
//
// The limiter is checked for every protected path, so discovery and metrics also
// count against a client's budget. Limits default to zero (disabled).
func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || isPublicPath(r.URL.Path) ||
			isMonitoringPath(r.URL.Path) || isAdminPath(r.URL.Path) || s.RateLimit == nil {
			// Monitoring must keep working when an API key is over budget.
			next.ServeHTTP(w, r)
			return
		}
		decision := s.RateLimit.Allow(clientIdentity(r))
		if decision.Allowed {
			next.ServeHTTP(w, r)
			return
		}

		if s.Metrics != nil {
			s.Metrics.ObserveRateLimited(decision.Reason)
		}
		seconds := int(decision.RetryAfter.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))

		message := "rate limit exceeded"
		if decision.Reason == "quota" {
			message = "daily token quota exhausted; the limit resets at 00:00 UTC"
		}
		log := LoggerFrom(r.Context())
		log.Warn("request rejected by rate limit",
			"reason", decision.Reason, "retry_after_s", seconds)
		if r.URL.Path == "/v1/messages" {
			writeAnthropicError(w, http.StatusTooManyRequests, message, "rate_limit_error")
			return
		}
		writeJSONError(w, http.StatusTooManyRequests, message, "rate_limit_error")
	})
}

// clientIdentity names the bucket a request is charged to.
//
// The API key is hashed so the raw credential never sits in a long-lived map; a
// trusted loopback client without a key shares the "local" bucket.
func clientIdentity(r *http.Request) string {
	if token := auth.ExtractToken(r); token != "" {
		sum := sha256.Sum256([]byte(token))
		return "key:" + hex.EncodeToString(sum[:8])
	}
	return "local"
}

// isMonitoringPath lists endpoints that never consume a client's rate budget.
func isMonitoringPath(p string) bool {
	return p == "/metrics" || p == "/usage"
}
