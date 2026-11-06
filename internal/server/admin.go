package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"lgllm/internal/admin"
	"lgllm/internal/config"
	"lgllm/internal/registry"
	"lgllm/internal/translate"
)

// --- authentication ----------------------------------------------------------

// isAdminPath lists the management API. These paths use admin.token, not the
// inference keys, so a client key cannot change providers.
func isAdminPath(p string) bool {
	switch p {
	case "/api/providers", "/api/connections", "/api/combos", "/api/keys",
		"/api/usage", "/api/logs", "/api/logs/stream", "/api/test-connection":
		return true
	}
	return strings.HasPrefix(p, "/api/providers/") ||
		strings.HasPrefix(p, "/api/connections/") ||
		strings.HasPrefix(p, "/api/combos/") ||
		strings.HasPrefix(p, "/api/keys/")
}

// withAdminAuth gates the management API separately from inference auth.
func (s *Server) withAdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		cfg := s.Config()
		if !cfg.Admin.Enabled || s.Admin == nil {
			writeJSONError(w, http.StatusNotFound, "admin API is disabled", "not_found")
			return
		}
		if cfg.Admin.Token != "" {
			got := adminToken(r)
			// Constant time to avoid leaking the token prefix.
			if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Admin.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="lgllm-admin"`)
				writeJSONError(w, http.StatusUnauthorized, "invalid or missing admin token", "invalid_admin_token")
				return
			}
		} else if !isLoopbackRemote(r) {
			writeJSONError(w, http.StatusForbidden,
				"admin API without a token is loopback-only", "admin_loopback_only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminToken reads the admin credential. Inference keys (Authorization without
// a value sent through the admin UI, x-api-key) are deliberately not accepted.
func adminToken(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("X-Admin-Token")); h != "" {
		return h
	}
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(tok)
		}
		if tok, ok := strings.CutPrefix(h, "bearer "); ok {
			return strings.TrimSpace(tok)
		}
	}
	return ""
}

// --- DTOs --------------------------------------------------------------------

type adminModel struct {
	ID           string          `json:"id"`
	Name         string          `json:"name,omitempty"`
	Context      int             `json:"context,omitempty"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}

type adminProvider struct {
	ID        string            `json:"id"`
	Alias     string            `json:"alias,omitempty"`
	BaseURL   string            `json:"base_url"`
	Format    string            `json:"format"`
	Auth      string            `json:"auth"`
	AuthParam string            `json:"auth_param,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	TimeoutMS int64             `json:"timeout_ms,omitempty"`
	Models    []adminModel      `json:"models,omitempty"`
	Managed   string            `json:"managed"` // config|admin
}

type adminConnection struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	APIKey   string `json:"api_key,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Managed  string `json:"managed"`
}

type adminCombo struct {
	Name       string   `json:"name"`
	Members    []string `json:"members"`
	Strategy   string   `json:"strategy,omitempty"`
	TokenSaver *bool    `json:"token_saver,omitempty"`
	Managed    string   `json:"managed"`
}

type adminKey struct {
	ID     string `json:"id"`
	Source string `json:"source"` // config|admin
}

func providerDTO(p config.Provider, managed string) adminProvider {
	out := adminProvider{
		ID: p.ID, Alias: p.Alias, BaseURL: p.BaseURL, Format: p.Format,
		Auth: p.Auth, AuthParam: p.AuthParam, Managed: managed,
		TimeoutMS: p.Timeout.Std().Milliseconds(),
	}
	if len(p.Headers) > 0 {
		out.Headers = make(map[string]string, len(p.Headers))
		for k, v := range p.Headers {
			out.Headers[k] = redactHeader(k, v)
		}
	}
	for _, m := range p.Models {
		out.Models = append(out.Models, adminModel{
			ID: m.ID, Name: m.Name, Context: m.Context, Capabilities: m.Capabilities,
		})
	}
	return out
}

func providerFromDTO(d adminProvider) config.Provider {
	p := config.Provider{
		ID: d.ID, Alias: d.Alias, BaseURL: d.BaseURL, Format: d.Format,
		Auth: d.Auth, AuthParam: d.AuthParam, Headers: d.Headers,
		Timeout: config.Duration(time.Duration(d.TimeoutMS) * time.Millisecond),
	}
	for _, m := range d.Models {
		p.Models = append(p.Models, config.Model{
			ID: m.ID, Name: m.Name, Context: m.Context, Capabilities: m.Capabilities,
		})
	}
	return p
}

func connectionDTO(c config.Connection, managed string) adminConnection {
	key := ""
	if c.APIKey != "" {
		key = "***"
	}
	return adminConnection{
		ID: c.ID, Provider: c.Provider, APIKey: key, BaseURL: c.BaseURL,
		Disabled: c.Disabled, Managed: managed,
	}
}

func comboDTO(c config.Combo, managed string) adminCombo {
	return adminCombo{
		Name: c.Name, Members: c.Members, Strategy: c.Strategy,
		TokenSaver: c.TokenSaver, Managed: managed,
	}
}

func redactHeader(key, value string) string {
	lower := strings.ToLower(key)
	for _, needle := range []string{"key", "token", "secret", "auth", "cookie"} {
		if strings.Contains(lower, needle) {
			if value == "" {
				return ""
			}
			return "***"
		}
	}
	return value
}

// --- providers ---------------------------------------------------------------

func (s *Server) handleAdminProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.Admin.Config()
		state := s.Admin.State()
		managed := make(map[string]bool, len(state.Providers))
		for _, p := range state.Providers {
			managed[p.ID] = true
		}
		out := make([]adminProvider, 0, len(cfg.Providers))
		for _, p := range cfg.Providers {
			source := "config"
			if managed[p.ID] {
				source = "admin"
			}
			out = append(out, providerDTO(p, source))
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
	case http.MethodPost:
		var dto adminProvider
		if err := decodeAdminBody(r, &dto); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if err := s.Admin.AddProvider(providerFromDTO(dto)); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		writeJSON(w, http.StatusCreated, providerDTO(providerFromDTO(dto), "admin"))
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
	}
}

func (s *Server) handleAdminProvider(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/providers/")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "provider id is required", "invalid_request_error")
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !s.Admin.HasProvider(id) {
			writeJSONError(w, http.StatusConflict, "provider is managed by config.yaml", "admin_readonly")
			return
		}
		var dto adminProvider
		if err := decodeAdminBody(r, &dto); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if err := s.Admin.UpdateProvider(id, providerFromDTO(dto)); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		writeJSON(w, http.StatusOK, providerDTO(providerFromDTO(dto), "admin"))
	case http.MethodDelete:
		if !s.Admin.HasProvider(id) {
			writeJSONError(w, http.StatusConflict, "provider is managed by config.yaml", "admin_readonly")
			return
		}
		if err := s.Admin.DeleteProvider(id); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
	}
}

// --- connections -------------------------------------------------------------

func (s *Server) handleAdminConnections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.Admin.Config()
		state := s.Admin.State()
		managed := make(map[string]bool, len(state.Connections))
		for _, c := range state.Connections {
			managed[c.ID] = true
		}
		out := make([]adminConnection, 0, len(cfg.Connections))
		for _, c := range cfg.Connections {
			source := "config"
			if managed[c.ID] {
				source = "admin"
			}
			out = append(out, connectionDTO(c, source))
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
	case http.MethodPost:
		var dto adminConnection
		if err := decodeAdminBody(r, &dto); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if err := s.Admin.AddConnection(connectionFromDTO(dto)); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		writeJSON(w, http.StatusCreated, connectionDTO(connectionFromDTO(dto), "admin"))
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
	}
}

func (s *Server) handleAdminConnection(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/connections/")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "connection id is required", "invalid_request_error")
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !s.Admin.HasConnection(id) {
			writeJSONError(w, http.StatusConflict, "connection is managed by config.yaml", "admin_readonly")
			return
		}
		var dto adminConnection
		if err := decodeAdminBody(r, &dto); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if err := s.Admin.UpdateConnection(id, connectionFromDTO(dto)); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		writeJSON(w, http.StatusOK, connectionDTO(connectionFromDTO(dto), "admin"))
	case http.MethodDelete:
		if !s.Admin.HasConnection(id) {
			writeJSONError(w, http.StatusConflict, "connection is managed by config.yaml", "admin_readonly")
			return
		}
		if err := s.Admin.DeleteConnection(id); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
	}
}

func connectionFromDTO(d adminConnection) config.Connection {
	return config.Connection{
		ID: d.ID, Provider: d.Provider, APIKey: d.APIKey,
		BaseURL: d.BaseURL, Disabled: d.Disabled,
	}
}

// --- combos ------------------------------------------------------------------

func (s *Server) handleAdminCombos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.Admin.Config()
		state := s.Admin.State()
		managed := make(map[string]bool, len(state.Combos))
		for _, c := range state.Combos {
			managed[c.Name] = true
		}
		out := make([]adminCombo, 0, len(cfg.Combos))
		for _, c := range cfg.Combos {
			source := "config"
			if managed[c.Name] {
				source = "admin"
			}
			out = append(out, comboDTO(c, source))
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
	case http.MethodPost:
		var dto adminCombo
		if err := decodeAdminBody(r, &dto); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if err := s.Admin.AddCombo(comboFromDTO(dto)); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		writeJSON(w, http.StatusCreated, comboDTO(comboFromDTO(dto), "admin"))
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
	}
}

func (s *Server) handleAdminCombo(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/combos/")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "combo name is required", "invalid_request_error")
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !s.Admin.HasCombo(name) {
			writeJSONError(w, http.StatusConflict, "combo is managed by config.yaml", "admin_readonly")
			return
		}
		var dto adminCombo
		if err := decodeAdminBody(r, &dto); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if err := s.Admin.UpdateCombo(name, comboFromDTO(dto)); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		writeJSON(w, http.StatusOK, comboDTO(comboFromDTO(dto), "admin"))
	case http.MethodDelete:
		if !s.Admin.HasCombo(name) {
			writeJSONError(w, http.StatusConflict, "combo is managed by config.yaml", "admin_readonly")
			return
		}
		if err := s.Admin.DeleteCombo(name); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
	}
}

func comboFromDTO(d adminCombo) config.Combo {
	return config.Combo{
		Name: d.Name, Members: d.Members, Strategy: d.Strategy, TokenSaver: d.TokenSaver,
	}
}

// --- keys --------------------------------------------------------------------

func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		state := s.Admin.State()
		cfg := s.Admin.Config()
		out := make([]adminKey, 0, len(state.Keys))
		for _, k := range state.Keys {
			out = append(out, adminKey{ID: admin.KeyID(k), Source: "admin"})
		}
		configKeys := len(cfg.Auth.Keys) - len(state.Keys)
		if configKeys < 0 {
			configKeys = 0
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object":      "list",
			"keys":        out,
			"config_keys": configKeys,
		})
	case http.MethodPost:
		var body struct {
			Key string `json:"key"`
		}
		if err := decodeAdminBody(r, &body); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if err := s.Admin.AddKey(body.Key); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error(), "admin_error")
			return
		}
		writeJSON(w, http.StatusCreated, adminKey{ID: admin.KeyID(body.Key), Source: "admin"})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
	}
}

func (s *Server) handleAdminKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "key id is required", "invalid_request_error")
		return
	}
	if err := s.Admin.DeleteKey(id); err != nil {
		writeJSONError(w, http.StatusConflict, err.Error(), "admin_error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- usage and logs ----------------------------------------------------------

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseWindow(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	group := r.URL.Query().Get("group")
	switch group {
	case "", "provider", "model", "day":
	default:
		writeJSONError(w, http.StatusBadRequest, "group must be provider|model|day", "invalid_request_error")
		return
	}
	if group == "" {
		group = "provider"
	}

	type bucket struct {
		Key              string  `json:"key"`
		Requests         int     `json:"requests"`
		Failures         int     `json:"failures"`
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		SavedBytes       int     `json:"saved_bytes"`
		CostUSD          float64 `json:"cost_usd"`
		CostKnown        bool    `json:"cost_known"`
	}
	agg := make(map[string]*bucket)
	for _, e := range s.Usage.Snapshot(0) {
		if !from.IsZero() && e.Time.Before(from) {
			continue
		}
		if !to.IsZero() && !e.Time.Before(to) {
			continue
		}
		key := "(unknown)"
		switch group {
		case "provider":
			if e.Provider != "" {
				key = e.Provider
			}
		case "model":
			key = strings.TrimPrefix(e.Provider+"/"+e.Model, "/")
		case "day":
			key = e.Time.UTC().Format("2006-01-02")
		}
		b := agg[key]
		if b == nil {
			b = &bucket{Key: key, CostKnown: true}
			agg[key] = b
		}
		b.Requests++
		b.PromptTokens += e.PromptTokens
		b.CompletionTokens += e.CompletionTokens
		b.SavedBytes += e.SavedBytes
		b.CostUSD += e.CostUSD
		if e.Status >= 400 {
			b.Failures++
		}
		if !e.CostKnown {
			b.CostKnown = false
		}
	}

	out := make([]bucket, 0, len(agg))
	for _, b := range agg {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"group":  group,
		"data":   out,
	})
}

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if s.Usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "entries": []any{}})
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer", "invalid_request_error")
			return
		}
		limit = n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":  "list",
		"entries": s.Usage.Snapshot(limit),
	})
}

// handleAdminLogsStream pushes new usage entries as server-sent events.
func (s *Server) handleAdminLogsStream(w http.ResponseWriter, r *http.Request) {
	if s.Usage == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "usage log is disabled", "admin_error")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming is not supported", "internal_error")
		return
	}
	events, cancel := s.Usage.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-events:
			if !ok {
				return
			}
			raw, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseWindow(r *http.Request) (from, to time.Time, err error) {
	parse := func(name string) (time.Time, error) {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("%s must be RFC3339", name)
		}
		return t, nil
	}
	if from, err = parse("from"); err != nil {
		return
	}
	to, err = parse("to")
	return
}

// --- test connection ---------------------------------------------------------

func (s *Server) handleAdminTestConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "not_found")
		return
	}
	var body struct {
		Provider   string `json:"provider"`
		Connection string `json:"connection"`
		Model      string `json:"model"`
	}
	if err := decodeAdminBody(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	reg := s.Registry()
	provider, ok := reg.Provider(body.Provider)
	if !ok {
		writeJSONError(w, http.StatusNotFound,
			"provider is unknown or has no usable credential", "admin_error")
		return
	}

	conn := provider.Connections()[0]
	if body.Connection != "" {
		found := false
		for _, c := range provider.Connections() {
			if c.ID == body.Connection {
				conn, found = c, true
				break
			}
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "connection not found on provider", "admin_error")
			return
		}
	}

	model := body.Model
	if model == "" && len(provider.Models) > 0 {
		model = provider.Models[0].ID
	}
	if model == "" {
		writeJSONError(w, http.StatusBadRequest, `"model" is required when the provider advertises none`, "invalid_request_error")
		return
	}

	pivot := &translate.Request{
		Model:     model,
		Messages:  []translate.Message{{Role: "user", Parts: []translate.Part{{Type: "text", Text: "ping"}}}},
		MaxTokens: 8,
	}
	payload, err := translate.EncodeRequest(provider.Format, pivot)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	base, err := provider.Endpoint(conn)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error(), "admin_error")
		return
	}
	endpoint, err := generationURL(provider.Format, base, model, false)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error(), "admin_error")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range provider.Headers {
		req.Header.Set(k, v)
	}
	if provider.Format == registry.FormatAnthropic && req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	applyProviderAuth(req, provider, conn)

	start := time.Now()
	resp, err := s.Upstream.Do(req, 15*time.Second)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "reachable": false, "provider": provider.ID,
			"connection": conn.ID, "model": model, "latency_ms": latency,
			"error": err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	out := map[string]any{
		"ok": resp.StatusCode >= 200 && resp.StatusCode < 300,
		// reachable means the provider answered, even with 401/404.
		"reachable":  true,
		"provider":   provider.ID,
		"connection": conn.ID,
		"model":      model,
		"status":     resp.StatusCode,
		"latency_ms": latency,
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		out["error"] = translate.ParseError(raw)
	}
	writeJSON(w, http.StatusOK, out)
}

func decodeAdminBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
