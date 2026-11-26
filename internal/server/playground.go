package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handleAdminPlaygroundModels lists only models the playground can actually
// chat with. Ollama and System One formats have no OpenAI Chat translator, so
// they would only produce a 501 and are left out.
func (s *Server) handleAdminPlaygroundModels(w http.ResponseWriter, r *http.Request) {
	reg := s.Registry()
	data := make([]modelObject, 0, 16)
	for _, p := range reg.Providers() {
		if !translatable(p.Format) {
			continue
		}
		for _, m := range p.Models {
			obj := modelObject{
				ID:      modelRef(p, m),
				Object:  "model",
				OwnedBy: p.ID,
			}
			if m.Context > 0 {
				obj.ContextLength = m.Context
				obj.ContextWindow = m.Context
				obj.ContextKnown = true
			}
			data = append(data, obj)
		}
	}
	for _, a := range reg.Aliases() {
		if !translatable(a.Target.Provider.Format) {
			continue
		}
		obj := modelObject{ID: a.Name, Object: "model", OwnedBy: a.Target.Provider.ID}
		if context, known := contextForTarget(a.Target); known {
			obj.ContextLength = context
			obj.ContextWindow = context
			obj.ContextKnown = true
		}
		data = append(data, obj)
	}
	for _, c := range reg.Combos() {
		usable := false
		for _, m := range c.Members {
			if translatable(m.Provider.Format) {
				usable = true
				break
			}
		}
		if !usable {
			continue
		}
		obj := modelObject{ID: c.Name, Object: "model", OwnedBy: "combo"}
		if context := contextForCombo(c); context.Known {
			obj.ContextLength = context.Context
			obj.ContextWindow = context.Context
			obj.ContextKnown = true
		}
		data = append(data, obj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleAdminPlaygroundChat feeds an admin-authored OpenAI Chat body through the
// same routing, fallback, translation and usage pipeline as a real client. The
// admin token is removed before serving, so the request is attributed to the
// local bucket and no credential leaks upstream.
//
// Streaming works because serve writes to the same ResponseWriter; the UI reads
// the SSE stream directly. Non-streaming responses use the normal JSON envelope.
func (s *Server) handleAdminPlaygroundChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTranslatedBodyBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read playground request: "+err.Error(), "invalid_request_error")
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(rawString(payload["model"])) == "" {
		writeJSONError(w, http.StatusBadRequest, `field "model" is required`, "invalid_request_error")
		return
	}
	var messages []json.RawMessage
	rawMessages, ok := payload["messages"]
	if !ok || json.Unmarshal(rawMessages, &messages) != nil || len(messages) == 0 {
		writeJSONError(w, http.StatusBadRequest, `field "messages" must be a non-empty array`, "invalid_request_error")
		return
	}

	// The admin middleware already authenticated this request. Strip every
	// credential header so serve() cannot mistake the admin token for a client
	// key or forward it upstream.
	r.Header.Del("Authorization")
	r.Header.Del("X-Api-Key")
	r.Header.Del("X-Admin-Token")
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	s.serve(w, r, ingressChat)
}
