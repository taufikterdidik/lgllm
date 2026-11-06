package server

import (
	"io"
	"net/http"

	"lgllm/internal/registry"
)

// maxErrorBodyBytes caps how much of an upstream error body is relayed. A
// provider error is a small JSON document; anything larger is a gateway page,
// and forwarding megabytes of HTML would only confuse the caller.
const maxErrorBodyBytes = 1 << 20

// upstreamErrorHeaders are the response headers worth keeping when relaying a
// provider error. Everything else is either hop-by-hop or describes a body we
// are rewriting.
var upstreamErrorHeaders = []string{
	"Content-Type",
	"Retry-After",
	"X-Request-Id",
	"X-RateLimit-Limit",
	"X-RateLimit-Remaining",
	"X-RateLimit-Reset",
}

// handleChatCompletions serves POST /v1/chat/completions.
//
// The handler is a thin wrapper: s.serve owns routing, translation and relay so
// /v1/messages shares the exact same path.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, ingressChat)
}

// handleAnthropicMessages serves POST /v1/messages, the dialect Anthropic
// clients speak. The response is always Anthropic-shaped, whatever the provider
// speaks internally.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, ingressAnthropic)
}

// relayUpstreamError forwards a provider error verbatim.
//
// The body is kept rather than replaced by a generic message because provider
// text ("context length exceeded", "invalid tool_call id") is exactly what the
// caller needs to fix the request. Only a curated header set is copied; framing
// headers must be recomputed by our own server.
func relayUpstreamError(w http.ResponseWriter, resp *http.Response) {
	for _, h := range upstreamErrorHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil && len(body) == 0 {
		writeJSONError(w, http.StatusBadGateway, "upstream error body was unreadable", "upstream_error")
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// applyProviderAuth installs the outbound credential for the provider's scheme.
//
// A "none" provider (local Ollama) is sent no credential at all; an empty key is
// still tolerated here because config validation is what rejects it.
func applyProviderAuth(req *http.Request, p *registry.Provider, c registry.Connection) {
	if p.Auth == "none" || c.APIKey == "" {
		return
	}
	switch p.Auth {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	case "x-api-key":
		req.Header.Set("x-api-key", c.APIKey)
	case "query":
		q := req.URL.Query()
		q.Set(p.AuthParam, c.APIKey)
		req.URL.RawQuery = q.Encode()
	}
}

// modelObject is one entry of the model list.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// modelRef is the client-facing model id: provider alias (or id) plus the
// upstream model id, e.g. "ds/deepseek-chat". Every listing endpoint uses it so
// a name taken from a list always works as a request's "model".
func modelRef(p *registry.Provider, m registry.Model) string {
	prefix := p.Alias
	if prefix == "" {
		prefix = p.ID
	}
	return prefix + "/" + m.ID
}

// handleModels lists every model the usable providers advertise.
//
// Ids are prefixed with the provider alias ("ds/deepseek-chat") so a client can
// send one straight back to /v1/chat/completions. The provider id works too;
// Resolve accepts both. This handler also serves /api/v1/models verbatim.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	reg := s.Registry()
	data := make([]modelObject, 0, 16)
	for _, p := range reg.Providers() {
		for _, m := range p.Models {
			data = append(data, modelObject{
				ID:      modelRef(p, m),
				Object:  "model",
				Created: 0,
				OwnedBy: p.ID,
			})
		}
	}

	// Aliases and combos are routable names too, so a client that only reads
	// this list can still use them. They are appended after the concrete models
	// because owned_by has no single answer for a combo.
	for _, a := range reg.Aliases() {
		data = append(data, modelObject{
			ID:      a.Name,
			Object:  "model",
			Created: 0,
			OwnedBy: a.Target.Provider.ID,
		})
	}
	for _, c := range reg.Combos() {
		data = append(data, modelObject{
			ID:      c.Name,
			Object:  "model",
			Created: 0,
			OwnedBy: "combo",
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}
