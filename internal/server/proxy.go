package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lgllm/internal/registry"
	"lgllm/internal/upstream"
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

// handleChatCompletions proxies POST /v1/chat/completions.
//
// Phase 2 is a passthrough: the body is forwarded in the chat-completions shape
// to a provider that speaks it natively, and the response — streamed or not — is
// relayed as it arrives. Providers on another wire format are refused with an
// explicit message until the translator lands in phase 4.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	log := LoggerFrom(r.Context())

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeBadRequest(w, r, err)
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeJSONError(w, http.StatusBadRequest, "request body is empty", "invalid_request_error")
		return
	}

	// Decode into raw messages: lgllm needs only "model", and every other field
	// (tools, response_format, vendor extensions) must survive untouched.
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		writeBadRequest(w, r, err)
		return
	}
	var requested string
	if raw, ok := payload["model"]; ok {
		if err := json.Unmarshal(raw, &requested); err != nil {
			writeBadRequest(w, r, errors.New(`"model" must be a string`))
			return
		}
	}
	if strings.TrimSpace(requested) == "" {
		writeJSONError(w, http.StatusBadRequest, `field "model" is required`, "invalid_request_error")
		return
	}
	var stream bool
	if raw, ok := payload["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}

	reg := s.Registry()
	if reg.Len() == 0 {
		// Distinct from a routing miss: the process is healthy but has nothing
		// to route to, which is an operator problem, not a client mistake.
		writeJSONError(w, http.StatusServiceUnavailable,
			"no provider is usable; add a connection with a credential to the config", "no_providers")
		return
	}

	provider, model, err := reg.Resolve(requested)
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, registry.ErrNoDefaultProvider) {
			status = http.StatusBadRequest
		}
		log.Warn("model routing failed", "model", requested, "err", err)
		writeJSONError(w, status, err.Error(), "invalid_request_error")
		return
	}
	if !provider.SupportsPassthrough() {
		writeJSONError(w, http.StatusNotImplemented,
			fmt.Sprintf("provider %q speaks the %q format, which needs the format translator (phase 4); use a chat-completions provider for now",
				provider.ID, provider.Format),
			"not_implemented")
		return
	}

	conn, err := provider.Connection()
	if err != nil {
		log.Error("provider has no usable connection", "provider", provider.ID, "err", err)
		writeJSONError(w, http.StatusBadGateway, err.Error(), "provider_unavailable")
		return
	}
	base, err := provider.Endpoint(conn)
	if err != nil {
		log.Error("provider endpoint is invalid", "provider", provider.ID, "err", err)
		writeJSONError(w, http.StatusBadGateway, err.Error(), "provider_misconfigured")
		return
	}

	// Only re-encode when the model id actually changes, so the common case
	// forwards the client's exact bytes.
	if model != requested {
		raw, err := json.Marshal(model)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "cannot encode model name", "internal_error")
			return
		}
		payload["model"] = raw
		if body, err = json.Marshal(payload); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "cannot re-encode request", "internal_error")
			return
		}
	}

	url := strings.TrimRight(base, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Error("cannot build upstream request", "url", url, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "invalid upstream endpoint", "internal_error")
		return
	}

	req.Header.Set("Content-Type", "application/json")
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	} else if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	// Attribution headers some providers require, plus the caller's user agent
	// because a few providers gate on known clients. The inbound Authorization
	// is never forwarded, so a client key cannot leak upstream.
	for _, h := range []string{"User-Agent", "HTTP-Referer", "X-Title"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	for k, v := range provider.Headers {
		req.Header.Set(k, v)
	}
	applyProviderAuth(req, provider, conn)

	start := time.Now()
	resp, err := s.Upstream.Do(req, provider.Timeout)
	if err != nil {
		// A client that hung up is not an upstream failure, and there is nobody
		// left to answer.
		if r.Context().Err() != nil {
			log.Info("client cancelled before the upstream answered",
				"provider", provider.ID, "model", model)
			return
		}
		status := upstream.TransportErrorStatus(err)
		log.Warn("upstream request failed",
			"provider", provider.ID, "model", model, "status", status, "err", err)
		writeJSONError(w, status, "upstream request failed: "+err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()

	// One custom header is how a user or a bug report identifies the hop.
	w.Header().Set("X-Lgllm-Provider", provider.ID)
	w.Header().Set("X-Lgllm-Model", model)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		relayUpstreamError(w, resp)
		log.Warn("upstream returned an error",
			"provider", provider.ID, "model", model, "status", resp.StatusCode,
			"dur_ms", time.Since(start).Milliseconds())
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)

	isSSE := strings.HasPrefix(contentType, "text/event-stream")
	if isSSE {
		// Intermediaries would otherwise buffer the whole stream: no-cache for
		// caches, X-Accel-Buffering for nginx.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	} else if resp.ContentLength > 0 {
		// A non-streamed body is copied whole, so the upstream length is still
		// accurate and the client can size its buffer instead of waiting for
		// chunked-encoding EOF.
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	w.WriteHeader(resp.StatusCode)
	if isSSE {
		// Send the headers before the first token: a client should learn the
		// stream opened immediately, and no intermediary should hold them back.
		_ = http.NewResponseController(w).Flush()
	}

	n, err := s.Upstream.Relay(w, resp, isSSE)
	attrs := []any{
		"provider", provider.ID,
		"model", model,
		"stream", isSSE,
		"status", resp.StatusCode,
		"bytes", n,
		"dur_ms", time.Since(start).Milliseconds(),
	}
	switch {
	case err == nil:
		log.Info("proxied chat completion", attrs...)
	case errors.Is(err, upstream.ErrIdle):
		log.Warn("upstream stream stalled; closed it", attrs...)
	case errors.Is(err, upstream.ErrWriteTimeout):
		log.Debug("client stopped reading; closed the stream", attrs...)
	case r.Context().Err() != nil:
		log.Info("client disconnected mid-stream", attrs...)
	default:
		log.Warn("stream ended with an error", append(attrs, "err", err)...)
	}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}
