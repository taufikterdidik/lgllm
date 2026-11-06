package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lgllm/internal/registry"
	"lgllm/internal/tokensaver"
	"lgllm/internal/translate"
	"lgllm/internal/upstream"
)

// generationRequest is everything the retry loop needs from the client request.
type generationRequest struct {
	in        ingress
	body      []byte
	payload   map[string]json.RawMessage
	requested string
	stream    bool
	// session makes connection choice sticky so an upstream prompt cache stays
	// warm across turns of the same conversation.
	session string

	// pivot is the decoded client body, present when translation or the token
	// saver needs it. stats/saving describe what the saver did.
	pivot  *translate.Request
	stats  tokensaver.Stats
	saving bool

	// start and requestID tie the final log line, metrics and usage entry to one
	// request.
	start     time.Time
	requestID string
}

// attemptFailure is one failed (provider, connection) pair, reported to the
// client when every attempt fails.
type attemptFailure struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Connection string `json:"connection,omitempty"`
	Status     int    `json:"status,omitempty"`
	Error      string `json:"error"`

	// header and body keep a provider error intact, so a single failed attempt
	// can be relayed verbatim as before retries existed.
	header http.Header
	body   []byte
}

// attemptOutcome reports whether the response has been written.
type attemptOutcome struct {
	done  bool
	fatal error
}

// runPlan walks the combo members (or the single target) and, within each, the
// provider's credentials. It retries until a response is committed.
func (s *Server) runPlan(w http.ResponseWriter, r *http.Request, req *generationRequest, plan registry.Plan) {
	log := LoggerFrom(r.Context())
	failures := make([]attemptFailure, 0, len(plan.Attempts))

	for i, attempt := range plan.Attempts {
		if r.Context().Err() != nil {
			return // client hung up; there is nobody to answer
		}
		if !translatable(attempt.Provider.Format) {
			s.Metrics.ObserveFailure(attempt.Provider.ID, http.StatusNotImplemented)
			failures = append(failures, attemptFailure{
				Provider: attempt.Provider.ID,
				Model:    attempt.Model,
				Status:   http.StatusNotImplemented,
				Error: fmt.Sprintf("format %q has no translator",
					attempt.Provider.Format),
			})
			continue
		}

		out := s.tryProvider(w, r, req, attempt, i < len(plan.Attempts)-1, &failures)
		if out.done {
			return
		}
		if out.fatal != nil {
			s.badRequest(w, r, req.in, out.fatal)
			return
		}
	}

	s.writeFailures(w, req, plan, failures)
	log.Warn("all attempts failed",
		"model", req.requested,
		"combo", plan.Combo,
		"attempts", len(failures),
	)
}

// tryProvider tries every usable credential of one provider, in the order the
// balancer chooses.
func (s *Server) tryProvider(w http.ResponseWriter, r *http.Request, req *generationRequest, attempt registry.Attempt, moreAttempts bool, failures *[]attemptFailure) attemptOutcome {
	provider := attempt.Provider

	outbound, err := buildOutbound(req, provider.Format, attempt.Model)
	if err != nil {
		// The client's body cannot be rendered for this provider; another
		// member would receive the same body, so this is final.
		return attemptOutcome{fatal: err}
	}

	conns := provider.Connections()
	keys := make([]string, len(conns))
	for i := range conns {
		keys[i] = conns[i].ID
	}
	order, healthy := s.Balancer.Order(provider.ID, keys, req.session)
	if !healthy && moreAttempts {
		// Every credential of this provider is cooling down. With another combo
		// member available, move on rather than replay a known-bad account.
		*failures = append(*failures, attemptFailure{
			Provider: provider.ID,
			Model:    attempt.Model,
			Status:   http.StatusServiceUnavailable,
			Error:    "all credentials are cooling down after a recent failure",
		})
		return attemptOutcome{}
	}

	for pos, ci := range order {
		if r.Context().Err() != nil {
			return attemptOutcome{done: true}
		}
		conn := conns[ci]
		// Peeking only matters when a fallback exists. With a single option the
		// old behaviour wins: send the headers at once, so a client can close the
		// body and cancel the upstream request.
		peek := moreAttempts || pos < len(order)-1
		done, failure := s.tryConnection(w, r, req, provider, conn, attempt.Model, outbound, peek)
		if done {
			return attemptOutcome{done: true}
		}
		if failure != nil {
			*failures = append(*failures, *failure)
		}
	}
	return attemptOutcome{}
}

// tryConnection performs one upstream call and either writes the response or
// returns the failure that should trigger the next attempt.
func (s *Server) tryConnection(
	w http.ResponseWriter,
	r *http.Request,
	req *generationRequest,
	provider *registry.Provider,
	conn registry.Connection,
	model string,
	outbound []byte,
	peek bool,
) (bool, *attemptFailure) {
	log := LoggerFrom(r.Context())

	base, err := provider.Endpoint(conn)
	if err != nil {
		log.Error("provider endpoint is invalid", "provider", provider.ID, "err", err)
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusBadGateway, Error: err.Error(),
		}
	}
	endpoint, err := generationURL(provider.Format, base, model, req.stream)
	if err != nil {
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusBadGateway, Error: err.Error(),
		}
	}

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(outbound))
	if err != nil {
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusInternalServerError, Error: err.Error(),
		}
	}
	upReq.Header.Set("Content-Type", "application/json")
	if accept := r.Header.Get("Accept"); accept != "" {
		upReq.Header.Set("Accept", accept)
	} else if req.stream {
		upReq.Header.Set("Accept", "text/event-stream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}
	// Attribution headers some providers require, plus the caller's user agent
	// because a few providers gate on known clients. The inbound Authorization
	// is never forwarded, so a client key cannot leak upstream.
	for _, h := range []string{"User-Agent", "HTTP-Referer", "X-Title", "Anthropic-Beta"} {
		if v := r.Header.Get(h); v != "" {
			upReq.Header.Set(h, v)
		}
	}
	for k, v := range provider.Headers {
		upReq.Header.Set(k, v)
	}
	if provider.Format == registry.FormatAnthropic && upReq.Header.Get("anthropic-version") == "" {
		// The Messages API rejects a request without this header. A custom
		// provider definition often forgets it; the catalog entry carries it.
		upReq.Header.Set("anthropic-version", "2023-06-01")
	}
	applyProviderAuth(upReq, provider, conn)

	start := time.Now()
	resp, err := s.Upstream.Do(upReq, provider.Timeout)
	if err != nil {
		if r.Context().Err() != nil {
			log.Info("client cancelled before the upstream answered",
				"provider", provider.ID, "model", model)
			return true, nil
		}
		status := upstream.TransportErrorStatus(err)
		s.Metrics.ObserveFailure(provider.ID, status)
		cooldown := s.Balancer.Fail(conn.ID, 30*time.Second, 10*time.Minute)
		log.Warn("upstream request failed",
			"provider", provider.ID, "model", model, "status", status,
			"cooldown_ms", cooldown.Milliseconds(), "err", err)
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: status, Error: err.Error(),
		}
	}
	defer resp.Body.Close()

	// Identify the hop even on an error, so a failed request is traceable. A
	// later attempt overwrites these before anything is written to the client.
	w.Header().Set("X-Lgllm-Provider", provider.ID)
	w.Header().Set("X-Lgllm-Model", model)
	if header := tokenSaverHeader(req); header != "" {
		w.Header().Set("X-Lgllm-Token-Saver", header)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		s.Metrics.ObserveFailure(provider.ID, resp.StatusCode)
		class := classifyStatus(resp.StatusCode, resp.Header)
		cooldown := s.Balancer.Fail(conn.ID, class.base, class.max)
		log.Warn("upstream returned an error",
			"provider", provider.ID, "model", model, "conn", conn.ID,
			"status", resp.StatusCode, "cooldown_ms", cooldown.Milliseconds(),
			"dur_ms", time.Since(start).Milliseconds())
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: resp.StatusCode, Error: translate.ParseError(raw),
			header: resp.Header, body: raw,
		}
	}

	sameDialect := (req.in == ingressChat && provider.Format == registry.FormatChat) ||
		(req.in == ingressAnthropic && provider.Format == registry.FormatAnthropic)
	needsTranslation := !sameDialect

	if req.stream {
		if peek {
			err = peekStream(r.Context(), resp, s.Upstream.Options().StreamIdleTimeout.Std())
		}
		if err != nil {
			s.Metrics.ObserveFailure(provider.ID, http.StatusBadGateway)
			cooldown := s.Balancer.Fail(conn.ID, 30*time.Second, 10*time.Minute)
			log.Warn("upstream stream started with an error or no data",
				"provider", provider.ID, "model", model, "conn", conn.ID,
				"cooldown_ms", cooldown.Milliseconds(), "err", err)
			return false, &attemptFailure{
				Provider: provider.ID, Model: model, Connection: conn.ID,
				Status: http.StatusBadGateway, Error: err.Error(),
			}
		}
		s.Balancer.Succeed(conn.ID)
		// A stream has no usage until the provider chooses to send it; counting
		// tokens here would require parsing every chunk. Phase 7 records zero.
		s.recordRequest(req, provider.ID, model, http.StatusOK, 0, 0, "")

		if !needsTranslation {
			n, relayErr := s.relayVerbatim(w, resp)
			logRelay(log, provider, model, start, n, relayErr, req.stats)
			return true, nil
		}
		stages := make([]translate.Converter, 0, 2)
		if provider.Format != registry.FormatChat {
			conv, ok := translate.NewStreamConverter(provider.Format, translate.StreamOptions{Model: model})
			if !ok {
				s.writeError(w, req.in, http.StatusNotImplemented,
					"stream translation is not available for this provider format", "not_implemented")
				return true, nil
			}
			stages = append(stages, conv)
		}
		if req.in == ingressAnthropic {
			stages = append(stages, translate.AnthropicIngressConverter(model))
		}
		n, relayErr := s.pipeConverted(w, r, resp, stages...)
		logRelay(log, provider, model, start, n, relayErr, req.stats)
		return true, nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTranslatedBodyBytes))
	if err != nil {
		s.Metrics.ObserveFailure(provider.ID, http.StatusBadGateway)
		cooldown := s.Balancer.Fail(conn.ID, 15*time.Second, 5*time.Minute)
		log.Warn("reading upstream response failed",
			"provider", provider.ID, "model", model, "conn", conn.ID,
			"cooldown_ms", cooldown.Milliseconds(), "err", err)
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusBadGateway, Error: "reading upstream response: " + err.Error(),
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		s.Metrics.ObserveFailure(provider.ID, http.StatusBadGateway)
		cooldown := s.Balancer.Fail(conn.ID, 15*time.Second, 5*time.Minute)
		log.Warn("upstream returned an empty response",
			"provider", provider.ID, "model", model, "conn", conn.ID,
			"cooldown_ms", cooldown.Milliseconds())
		return false, &attemptFailure{
			Provider: provider.ID, Model: model, Connection: conn.ID,
			Status: http.StatusBadGateway, Error: "upstream returned an empty response",
		}
	}

	var (
		out   []byte
		usage translate.Usage
	)
	if needsTranslation {
		pivot, err := translate.DecodeResponse(provider.Format, raw)
		if err != nil {
			s.Metrics.ObserveFailure(provider.ID, http.StatusBadGateway)
			cooldown := s.Balancer.Fail(conn.ID, 15*time.Second, 5*time.Minute)
			log.Warn("decoding upstream response failed",
				"provider", provider.ID, "model", model, "conn", conn.ID,
				"cooldown_ms", cooldown.Milliseconds(), "err", err)
			return false, &attemptFailure{
				Provider: provider.ID, Model: model, Connection: conn.ID,
				Status: http.StatusBadGateway, Error: "decoding upstream response: " + err.Error(),
			}
		}
		if req.in == ingressAnthropic {
			out, err = translate.EncodeAnthropicResponse(pivot)
		} else {
			out, err = translate.EncodeOpenAI(pivot)
		}
		if err != nil {
			return false, &attemptFailure{
				Provider: provider.ID, Model: model, Connection: conn.ID,
				Status: http.StatusBadGateway, Error: "encoding response: " + err.Error(),
			}
		}
		usage = pivot.Usage
	} else {
		out = raw
		// Metrics only: a body we cannot parse still reaches the client
		// untouched, it just contributes no token counts.
		if parsed, perr := translate.DecodeResponse(provider.Format, raw); perr == nil {
			usage = parsed.Usage
		}
	}

	s.Balancer.Succeed(conn.ID)
	s.recordRequest(req, provider.ID, model, http.StatusOK,
		usage.PromptTokens, usage.CompletionTokens, "")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(out); err != nil {
		log.Debug("client stopped reading the response", "err", err)
	}
	log.Info("proxied completion",
		"provider", provider.ID,
		"model", model,
		"conn", conn.ID,
		"stream", false,
		"status", http.StatusOK,
		"bytes", len(out),
		"saved_bytes", req.stats.Saved,
		"dur_ms", time.Since(start).Milliseconds(),
	)
	return true, nil
}

// failureClass is the cooldown a failed attempt earns.
type failureClass struct {
	base time.Duration
	max  time.Duration
}

// classifyStatus decides whether a failure is request-scoped or account-scoped.
//
// A bad parameter (400/404/413/422) says nothing about the credential, so it
// gets no cooldown but still falls through to the next member. Auth, quota,
// rate limit and upstream failures cool that credential down.
func classifyStatus(status int, h http.Header) failureClass {
	switch {
	case status == 400 || status == 404 || status == 413 || status == 422:
		return failureClass{}
	case status == 401 || status == 403:
		return failureClass{base: 30 * time.Minute, max: 2 * time.Hour}
	case status == 402:
		return failureClass{base: 15 * time.Minute, max: 24 * time.Hour}
	case status == 429:
		if d := parseRetryAfter(h.Get("Retry-After")); d > 0 {
			return failureClass{base: d, max: d}
		}
		return failureClass{base: time.Second, max: 4 * time.Minute}
	case status == 408:
		return failureClass{base: 15 * time.Second, max: 5 * time.Minute}
	case status >= 500:
		return failureClass{base: 30 * time.Second, max: 10 * time.Minute}
	default:
		return failureClass{base: 10 * time.Second, max: 5 * time.Minute}
	}
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// peekStream reads the first server-sent event before any byte reaches the
// client. An upstream that answers 200 and then sends an error (or nothing) has
// not committed the request, so the caller may still try the next member.
func peekStream(ctx context.Context, resp *http.Response, idle time.Duration) error {
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var buf bytes.Buffer

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// A client that hung up must abort the peek, otherwise this
			// goroutine waits on a provider that will never speak.
			_ = resp.Body.Close()
		case <-done:
		}
	}()

	if idle > 0 {
		timer := time.AfterFunc(idle, func() { _ = resp.Body.Close() })
		defer timer.Stop()
	}

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			buf.WriteString(line)
			trimmed := strings.TrimSpace(line)
			if payload, ok := strings.CutPrefix(trimmed, "data:"); ok {
				payload = strings.TrimSpace(payload)
				switch {
				case payload == "":
					// keep-alive or a multi-line event; keep reading
				case payload == "[DONE]":
					return errors.New("upstream stream ended before any content")
				default:
					if msg := streamError(buf.String(), payload); msg != "" {
						return errors.New(msg)
					}
					resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf.Bytes()), br))
					return nil
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("upstream stream ended before any content")
			}
			return err
		}
	}
}

// streamError reports a provider error encoded inside a 200 SSE stream.
func streamError(prefix, payload string) string {
	if strings.Contains(prefix, "event: error") {
		return payloadMessage(payload)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &m) != nil {
		return ""
	}
	if raw, ok := m["error"]; ok && string(raw) != "null" {
		return rawErrorMessage(raw)
	}
	if dtype := rawString(m["type"]); dtype == "error" {
		return payloadMessage(payload)
	}
	if rawString(m["object"]) == "error" {
		return rawErrorMessage(m["error"])
	}
	return ""
}

func rawErrorMessage(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return strings.TrimSpace(string(raw))
}

func payloadMessage(payload string) string {
	var obj struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(payload), &obj) == nil {
		if obj.Error.Message != "" {
			return obj.Error.Message
		}
		if obj.Message != "" {
			return obj.Message
		}
	}
	return payload
}

// writeFailures reports every attempt when none succeeded.
//
// A single attempt keeps the provider's status and body verbatim, which is what
// a caller sees without retries. Several attempts get a 502 summary that names
// each provider/model and its error, so debugging does not require the logs.
func (s *Server) writeFailures(w http.ResponseWriter, req *generationRequest, plan registry.Plan, failures []attemptFailure) {
	if len(failures) == 0 {
		s.recordRequest(req, "", req.requested, http.StatusBadGateway, 0, 0, "no attempt could be made")
		s.writeError(w, req.in, http.StatusBadGateway, "no attempt could be made", "upstream_error")
		return
	}

	last := failures[len(failures)-1]
	finalStatus := http.StatusBadGateway
	if len(failures) == 1 {
		finalStatus = last.Status
		if finalStatus == 0 {
			finalStatus = http.StatusBadGateway
		}
	}
	summary := last.Error
	if len(failures) > 1 {
		summary = planSummary(failures)
	}
	s.recordRequest(req, last.Provider, last.Model, finalStatus, 0, 0, summary)

	if len(failures) == 1 {
		f := failures[0]
		if f.body != nil && req.in != ingressAnthropic {
			for _, h := range upstreamErrorHeaders {
				if v := f.header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(f.Status)
			_, _ = w.Write(f.body)
			return
		}
		status := f.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		s.writeError(w, req.in, status, f.Error, "upstream_error")
		return
	}

	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		parts = append(parts, fmt.Sprintf("%s/%s: %s", f.Provider, f.Model, f.Error))
	}
	message := fmt.Sprintf("all %d attempt(s) failed for %q: %s",
		len(failures), req.requested, strings.Join(parts, "; "))

	if req.in == ingressAnthropic {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "api_error",
				"message": message,
			},
			"attempts": failures,
		})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
		},
		"attempts": failures,
	})
}

// sessionKey finds the conversation identifier used for sticky credentials.
//
// Claude Code and some OpenAI clients send an explicit header; Anthropic also
// carries metadata.user_id. Any stable key is enough to keep a prompt cache on
// one account, so the first available one wins.
func sessionKey(r *http.Request, payload map[string]json.RawMessage) string {
	for _, h := range []string{"X-Lgllm-Session", "X-Session-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	var meta struct {
		UserID string `json:"user_id"`
	}
	if raw, ok := payload["metadata"]; ok {
		_ = json.Unmarshal(raw, &meta)
		if meta.UserID != "" {
			return meta.UserID
		}
	}
	return rawString(payload["prompt_cache_key"])
}

// planSummary builds the one-line reason returned when every attempt failed.
func planSummary(failures []attemptFailure) string {
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		parts = append(parts, fmt.Sprintf("%s/%s: %s", f.Provider, f.Model, f.Error))
	}
	return strings.Join(parts, "; ")
}
