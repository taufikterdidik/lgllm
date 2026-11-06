package server

import (
	"net/http"

	"lgllm/internal/version"
)

// routes declares every endpoint.
//
// Go 1.22 method patterns are used throughout, so a wrong method yields 405
// automatically. The "/v1/" and "/api/" registrations are catch-alls that give
// an unknown API path a JSON error instead of an HTML 404.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /version", s.handleVersion)

	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /usage", s.handleUsage)

	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("POST /v1/messages", s.handleAnthropicMessages)

	// Compatibility endpoints: the same data in the dialect another server
	// would speak, so a client that probes these can discover what to route.
	// /api/v1/models is a pure alias — the same handler serves both paths, so
	// the two payloads cannot drift apart.
	s.mux.HandleFunc("GET /api/v1/models", s.handleModels)
	s.mux.HandleFunc("GET /api/tags", s.handleAPITags) // Ollama model list
	s.mux.HandleFunc("GET /api/show", s.handleAPIShow) // Ollama model details
	s.mux.HandleFunc("POST /api/show", s.handleAPIShow)
	s.mux.HandleFunc("GET /v1/props", s.handleProps) // llama.cpp server properties
	s.mux.HandleFunc("GET /props", s.handleProps)    // llama.cpp native path

	s.mux.HandleFunc("/v1/", s.handleUnknownEndpoint)
	s.mux.HandleFunc("/api/", s.handleUnknownEndpoint)

	s.mux.HandleFunc("/", s.handleRoot)
}

// handleHealthz is a liveness probe: it answers for as long as the process runs.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": version.String(),
	})
}

// handleReadyz is a readiness probe. It reports the active config and how many
// providers can actually serve a request; a proxy with zero usable providers is
// running but not useful.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	providers := s.Registry().Len()
	status := "ready"
	code := http.StatusOK
	if providers == 0 {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status":      status,
		"config":      cfg.Source,
		"fingerprint": cfg.Fingerprint(),
		"auth":        cfg.Auth.Mode,
		"providers":   providers,
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version.Version,
		"commit":  version.Commit,
		"date":    version.Date,
	})
}

// handleRoot gives a human a starting point instead of a bare 404.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSONError(w, http.StatusNotFound, "not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "lgllm",
		"version": version.String(),
		"endpoints": []string{
			"GET /healthz",
			"GET /readyz",
			"GET /version",
			"GET /metrics",
			"GET /usage",
			"GET /v1/models",
			"GET /api/v1/models",
			"GET /api/tags",
			"GET,POST /api/show",
			"GET /v1/props",
			"GET /props",
			"POST /v1/chat/completions",
			"POST /v1/messages",
		},
	})
}

// handleUnknownEndpoint keeps the /v1 and /api namespaces honest: an
// unimplemented endpoint gets a JSON 404 that names it, not an HTML page from
// the mux (and not the misleading "not found" of the root fallback).
func (s *Server) handleUnknownEndpoint(w http.ResponseWriter, r *http.Request) {
	writeJSONError(w, http.StatusNotFound,
		"unknown endpoint "+r.Method+" "+r.URL.Path, "not_found")
}
