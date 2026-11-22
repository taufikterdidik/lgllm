package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"lgllm/internal/registry"
	"lgllm/internal/upstream"
)

// maxSystemOneBodyBytes caps the decision response. Answers are small JSON
// documents; a larger body is a gateway error page, not something to buffer.
const maxSystemOneBodyBytes = 1 << 20

// handleSystemOne proxies the TypeSafe System One shape. It is deliberately
// separate from /v1/chat/completions: a decision request carries a state plus
// typed questions and never produces chat text or a stream.
func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	log := LoggerFrom(r.Context())
	start := time.Now()
	s.Metrics.IncInflight()
	defer s.Metrics.DecInflight()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.badRequest(w, r, ingressSystemOne, err)
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		s.writeError(w, ingressSystemOne, http.StatusBadRequest, "request body is empty", "invalid_request_error")
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		s.badRequest(w, r, ingressSystemOne, err)
		return
	}
	requested := strings.TrimSpace(rawString(payload["model"]))
	if requested == "" {
		s.writeError(w, ingressSystemOne, http.StatusBadRequest, `field "model" is required`, "invalid_request_error")
		return
	}
	if raw, ok := payload["stream"]; ok {
		var stream bool
		if json.Unmarshal(raw, &stream) == nil && stream {
			s.writeError(w, ingressSystemOne, http.StatusBadRequest,
				"System One does not support streaming", "invalid_request_error")
			return
		}
	}

	reg := s.Registry()
	if reg.Len() == 0 {
		s.writeError(w, ingressSystemOne, http.StatusServiceUnavailable,
			"no provider is usable; add a connection with a credential to the config", "no_providers")
		return
	}
	plan, err := reg.Plan(requested)
	if err != nil {
		log.Warn("systemone routing failed", "model", requested, "err", err)
		s.writeError(w, ingressSystemOne, http.StatusNotFound, err.Error(), "invalid_request_error")
		return
	}

	// A combo may mix chat and decision providers. Only the System One members
	// can serve this ingress; the rest would receive an incompatible body.
	attempts := make([]registry.Attempt, 0, len(plan.Attempts))
	for _, attempt := range plan.Attempts {
		if attempt.Provider.Format == registry.FormatSystemOne {
			attempts = append(attempts, attempt)
		}
	}
	if len(attempts) == 0 {
		s.writeError(w, ingressSystemOne, http.StatusBadRequest,
			"model is not a System One model; use /v1/chat/completions", "invalid_request_error")
		return
	}
	plan.Attempts = attempts

	requestID := RequestID(r.Context())
	clientKey := clientIdentity(r)
	req := &generationRequest{
		in:           ingressSystemOne,
		body:         body,
		payload:      payload,
		requested:    requested,
		session:      sessionKey(r, payload),
		conversation: conversationSession(r, payload, requestID, clientKey),
		start:        start,
		requestID:    requestID,
		clientKey:    clientKey,
	}
	s.runSystemOnePlan(w, r, req, plan)
}

func (s *Server) runSystemOnePlan(w http.ResponseWriter, r *http.Request, req *generationRequest, plan registry.Plan) {
	log := LoggerFrom(r.Context())
	failures := make([]attemptFailure, 0, len(plan.Attempts))
	req.combo = plan.Combo

	for i, attempt := range plan.Attempts {
		if r.Context().Err() != nil {
			return
		}
		req.comboMember = attempt.Member
		before := len(failures)
		out := s.trySystemOneProvider(w, r, req, attempt, i < len(plan.Attempts)-1, &failures)
		if out.done {
			return
		}
		if out.fatal != nil {
			s.badRequest(w, r, ingressSystemOne, out.fatal)
			return
		}
		s.recordComboFailure(plan.Combo, attempt.Member, failures[before:])
	}

	s.writeFailures(w, req, plan, failures)
	log.Warn("all systemone attempts failed",
		"model", req.requested,
		"combo", plan.Combo,
		"attempts", len(failures),
	)
}

func (s *Server) trySystemOneProvider(w http.ResponseWriter, r *http.Request, req *generationRequest, attempt registry.Attempt, moreAttempts bool, failures *[]attemptFailure) attemptOutcome {
	provider := attempt.Provider
	conns := provider.Connections()
	if len(conns) == 0 {
		*failures = append(*failures, attemptFailure{
			Provider: provider.ID, Model: attempt.Model,
			Status: http.StatusServiceUnavailable, Error: "provider has no enabled connection",
		})
		return attemptOutcome{}
	}
	keys := make([]string, len(conns))
	for i := range conns {
		keys[i] = conns[i].ID
	}
	order, healthy := s.Balancer.Order(provider.ID, keys, req.session)
	if !healthy && moreAttempts {
		*failures = append(*failures, attemptFailure{
			Provider: provider.ID, Model: attempt.Model,
			Status: http.StatusServiceUnavailable,
			Error:  "all credentials are cooling down after a recent failure",
		})
		return attemptOutcome{}
	}

	attempted, limited := false, 0
	for _, ci := range order {
		if r.Context().Err() != nil {
			return attemptOutcome{done: true}
		}
		conn := conns[ci]
		release, ok := s.ConnLimit.TryAcquire(conn.ID, conn.MaxConcurrent)
		if !ok {
			limited++
			s.Metrics.ObserveConnectionLimited(provider.ID, conn.ID)
			continue
		}
		attempted = true
		done, failure := s.trySystemOneConnection(w, r, req, provider, conn, attempt.Model)
		release()
		if done {
			return attemptOutcome{done: true}
		}
		if failure != nil {
			*failures = append(*failures, *failure)
		}
	}
	if !attempted && limited > 0 {
		*failures = append(*failures, attemptFailure{
			Provider: provider.ID, Model: attempt.Model,
			Status: http.StatusTooManyRequests,
			Error:  "all credentials are at their concurrency limit",
		})
	}
	return attemptOutcome{}
}

func (s *Server) trySystemOneConnection(w http.ResponseWriter, r *http.Request, req *generationRequest, provider *registry.Provider, conn registry.Connection, model string) (bool, *attemptFailure) {
	log := LoggerFrom(r.Context())
	base, err := provider.Endpoint(conn)
	if err != nil {
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusBadGateway, Error: err.Error(),
		}
	}
	outbound, err := rewriteModel(req.payload, model)
	if err != nil {
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusBadGateway, Error: err.Error(),
		}
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, systemOneEndpoint(base), bytes.NewReader(outbound))
	if err != nil {
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusInternalServerError, Error: err.Error(),
		}
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/json")
	applyUserAgent(upReq, r)
	for k, v := range provider.Headers {
		upReq.Header.Set(k, v)
	}
	applySessionHeader(upReq, provider, req.conversation)
	applyProviderAuth(upReq, provider, conn)

	start := time.Now()
	resp, err := s.Upstream.Do(upReq, provider.Timeout)
	if err != nil {
		if r.Context().Err() != nil {
			log.Info("client cancelled before systemone answered",
				"provider", provider.ID, "model", model)
			return true, nil
		}
		status := upstream.TransportErrorStatus(err)
		s.Metrics.ObserveFailure(provider.ID, status)
		cooldown := s.Balancer.Fail(conn.ID, 30*time.Second, 10*time.Minute)
		log.Warn("systemone request failed",
			"provider", provider.ID, "model", model, "status", status,
			"cooldown_ms", cooldown.Milliseconds(), "err", err,
			"dur_ms", time.Since(start).Milliseconds())
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: status, Error: err.Error(),
		}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSystemOneBodyBytes))
	if readErr != nil {
		s.Metrics.ObserveFailure(provider.ID, http.StatusBadGateway)
		cooldown := s.Balancer.Fail(conn.ID, 15*time.Second, 5*time.Minute)
		log.Warn("systemone body read failed",
			"provider", provider.ID, "model", model, "conn", conn.ID,
			"cooldown_ms", cooldown.Milliseconds(), "err", readErr)
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusBadGateway, Error: readErr.Error(),
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		s.Metrics.ObserveFailure(provider.ID, resp.StatusCode)
		class := classifyStatus(resp.StatusCode, resp.Header)
		cooldown := s.Balancer.Fail(conn.ID, class.base, class.max)
		log.Warn("systemone returned an error",
			"provider", provider.ID, "model", model, "conn", conn.ID,
			"status", resp.StatusCode, "cooldown_ms", cooldown.Milliseconds())
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: resp.StatusCode, Error: decisionErrorMessage(body), header: resp.Header, body: body,
		}
	}

	s.Balancer.Succeed(conn.ID)
	var meta struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &meta)
	s.recordRequest(req, provider.ID, model, resp.StatusCode,
		meta.Usage.InputTokens, meta.Usage.OutputTokens, "")

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Lgllm-Provider", provider.ID)
	w.Header().Set("X-Lgllm-Model", model)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return true, nil
}

// systemOneEndpoint appends /systemone once, so a connection override that
// already points at the full endpoint still works.
func systemOneEndpoint(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/systemone") {
		return base
	}
	return base + "/systemone"
}

func decisionErrorMessage(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if payload.Error.Message != "" {
			return payload.Error.Message
		}
		if payload.Message != "" {
			return payload.Message
		}
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		if len(text) > 300 {
			text = text[:300] + "..."
		}
		return text
	}
	return "upstream returned an error"
}
