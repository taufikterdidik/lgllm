package server

import (
	"net/http"

	"lgllm/internal/version"
)

// routes declares every endpoint.
//
// Go 1.22 method patterns are used throughout, so a wrong method yields 405
// automatically. The bare "/v1/" registration is the catch-all that gives an
// unknown API path a JSON error instead of an HTML 404.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /version", s.handleVersion)

	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)

	s.mux.HandleFunc("/v1/", s.handleUnknownV1)

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
			"GET /v1/models",
			"POST /v1/chat/completions",
		},
	})
}

// handleUnknownV1 keeps the /v1 namespace honest: an unimplemented endpoint gets
// a JSON 404 that names it, not an HTML page from the mux.
func (s *Server) handleUnknownV1(w http.ResponseWriter, r *http.Request) {
	writeJSONError(w, http.StatusNotFound,
		"unknown endpoint "+r.Method+" "+r.URL.Path, "not_found")
}
