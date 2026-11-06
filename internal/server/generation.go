package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lgllm/internal/registry"
	"lgllm/internal/tokensaver"
	"lgllm/internal/translate"
	"lgllm/internal/upstream"
)

// maxTranslatedBodyBytes caps a translated response body. Providers return one
// JSON document; a larger body is an HTML gateway page that would fail to parse
// anyway, and holding it in memory has no benefit.
const maxTranslatedBodyBytes = 8 << 20

// ingress is the dialect the client used. It decides the response shape and the
// error envelope, not the routing.
type ingress int

const (
	ingressChat ingress = iota
	ingressAnthropic
)

// serve handles /v1/chat/completions and /v1/messages.
//
// The routing, auth, timeout and relay path is shared. only the request body in
// and the response body out change with the dialect and the provider format:
// OpenAI Chat Completions is the pivot every other format is translated to.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, in ingress) {
	log := LoggerFrom(r.Context())
	start := time.Now()
	s.Metrics.IncInflight()
	defer s.Metrics.DecInflight()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.badRequest(w, r, in, err)
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		s.writeError(w, in, http.StatusBadRequest, "request body is empty", "invalid_request_error")
		return
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		s.badRequest(w, r, in, err)
		return
	}
	if log.Enabled(r.Context(), slog.LevelDebug) {
		// Bodies can contain prompts and pasted secrets; redact before logging
		// and cap the record so debug output stays usable.
		log.Debug("request body", "ingress", ingressName(in), "body", redactBody(body))
	}
	requested := rawString(payload["model"])
	if strings.TrimSpace(requested) == "" {
		s.writeError(w, in, http.StatusBadRequest, `field "model" is required`, "invalid_request_error")
		return
	}
	stream := false
	if raw, ok := payload["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}

	reg := s.Registry()
	if reg.Len() == 0 {
		s.writeError(w, in, http.StatusServiceUnavailable,
			"no provider is usable; add a connection with a credential to the config", "no_providers")
		return
	}

	plan, err := reg.Plan(requested)
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, registry.ErrNoDefaultProvider) {
			status = http.StatusBadRequest
		}
		log.Warn("model routing failed", "model", requested, "err", err)
		s.writeError(w, in, status, err.Error(), "invalid_request_error")
		return
	}
	log.Debug("routing plan",
		"requested", requested,
		"attempts", len(plan.Attempts),
		"alias", plan.Alias,
		"combo", plan.Combo,
		"ingress", ingressName(in),
	)

	ts := s.Config().TokenSaver
	saving := ts.Enabled && !plan.TokenSaverOff && !tokenSaverBypassed(r)
	needPivot := saving
	if !needPivot {
		for _, attempt := range plan.Attempts {
			if attempt.Provider.Format != ingressFormat(in) {
				needPivot = true
				break
			}
		}
	}

	var (
		pivot *translate.Request
		stats tokensaver.Stats
	)
	if needPivot {
		pivot, err = decodeIngress(in, body)
		if err != nil {
			s.badRequest(w, r, in, err)
			return
		}
		if saving {
			stats = tokensaver.Apply(pivot, tokensaver.Options{
				MinBytes: ts.MinBytes,
				MaxBytes: ts.MaxBytes,
				Prompt:   ts.Prompt,
			})
			stats.SystemChanged = ts.Prompt != ""
			if stats.Saved > 0 {
				log.Info("token saver compressed tool results",
					"before_bytes", stats.Before,
					"after_bytes", stats.After,
					"saved_bytes", stats.Saved,
					"filters", tokensaver.SummarizeFilters(stats.Filters))
			}
		}
	}

	req := &generationRequest{
		in:        in,
		body:      body,
		payload:   payload,
		requested: requested,
		stream:    stream,
		session:   sessionKey(r, payload),
		pivot:     pivot,
		stats:     stats,
		saving:    saving,
		start:     start,
		requestID: RequestID(r.Context()),
		clientKey: clientIdentity(r),
	}
	s.runPlan(w, r, req, plan)
}

// buildOutbound renders the upstream request body.
//
// The client body is decoded once (when translation or token saving needs the
// pivot) and re-encoded per attempt, because each combo member may speak a
// different format. When the provider matches the client dialect and the saver
// changed nothing, the original bytes are forwarded unchanged.
func buildOutbound(req *generationRequest, format, model string) ([]byte, error) {
	sameDialect := format == ingressFormat(req.in)
	if sameDialect && (req.pivot == nil || (!req.stats.Applied && !req.stats.SystemChanged)) {
		if model == req.requested {
			return req.body, nil // keep the client's bytes exactly
		}
		return rewriteModel(req.payload, model)
	}
	if req.pivot == nil {
		return nil, errors.New("translation requested without a decoded body")
	}

	req.pivot.Model = model
	req.pivot.Stream = req.stream
	encoded, err := translate.EncodeRequest(format, req.pivot)
	if err != nil {
		return nil, err
	}
	if !sameDialect {
		return encoded, nil
	}
	// Same dialect but the saver touched messages/system: splice only those
	// members into the client's original document, so vendor-specific top-level
	// fields (response_format, parallel_tool_calls, ...) survive.
	return mergeSameDialect(req.in, req.body, encoded, model)
}

// mergeSameDialect replaces the members a translation can change while keeping
// every unknown field the client sent.
func mergeSameDialect(in ingress, original, encoded []byte, model string) ([]byte, error) {
	var orig, enc map[string]json.RawMessage
	if err := json.Unmarshal(original, &orig); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, &enc); err != nil {
		return nil, err
	}
	if v, ok := enc["messages"]; ok {
		orig["messages"] = v
	}
	if in == ingressAnthropic {
		if v, ok := enc["system"]; ok {
			orig["system"] = v
		} else {
			delete(orig, "system")
		}
	}
	rawModel, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	orig["model"] = rawModel
	return json.Marshal(orig)
}

// decodeIngress parses the client's body in the dialect it arrived in.
func decodeIngress(in ingress, body []byte) (*translate.Request, error) {
	if in == ingressAnthropic {
		return translate.DecodeAnthropic(body)
	}
	return translate.DecodeOpenAI(body)
}

// ingressFormat is the registry format matching what the client speaks.
func ingressFormat(in ingress) string {
	if in == ingressAnthropic {
		return registry.FormatAnthropic
	}
	return registry.FormatChat
}

// tokenSaverBypassed reports the per-request opt-out.
func tokenSaverBypassed(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("X-Lgllm-Token-Saver"))) {
	case "off", "false", "0", "no":
		return true
	}
	return false
}

// generationURL is the provider's generation endpoint.
func generationURL(format, base, model string, stream bool) (string, error) {
	base = strings.TrimRight(base, "/")
	switch format {
	case registry.FormatChat:
		return base + "/chat/completions", nil
	case registry.FormatAnthropic:
		return base + "/messages", nil
	case registry.FormatResponses:
		return base + "/responses", nil
	case registry.FormatGemini:
		action := "generateContent"
		if stream {
			return base + "/models/" + url.PathEscape(model) + ":streamGenerateContent?alt=sse", nil
		}
		return base + "/models/" + url.PathEscape(model) + ":" + action, nil
	default:
		return "", fmt.Errorf("provider format %q has no generation endpoint", format)
	}
}

// relayVerbatim copies the upstream response as it arrives.
func (s *Server) relayVerbatim(w http.ResponseWriter, resp *http.Response) (int64, error) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)

	isSSE := strings.HasPrefix(contentType, "text/event-stream")
	if isSSE {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	} else if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	w.WriteHeader(resp.StatusCode)
	if isSSE {
		_ = http.NewResponseController(w).Flush()
	}
	return s.Upstream.Relay(w, resp, isSSE)
}

// logRelay records one stream or passthrough response.
func logRelay(log *slog.Logger, provider *registry.Provider, model string, start time.Time, n int64, err error, stats tokensaver.Stats) {
	attrs := []any{
		"provider", provider.ID,
		"model", model,
		"bytes", n,
		"saved_bytes", stats.Saved,
		"dur_ms", time.Since(start).Milliseconds(),
	}
	switch {
	case err == nil:
		log.Info("proxied completion", attrs...)
	case errors.Is(err, upstream.ErrIdle):
		log.Warn("upstream stream stalled; closed it", attrs...)
	case errors.Is(err, upstream.ErrWriteTimeout):
		log.Debug("client stopped reading; closed the stream", attrs...)
	default:
		log.Warn("stream ended with an error", append(attrs, "err", err)...)
	}
}

// pipeConverted runs the translation stages over the upstream body and relays
// the result with the same idle and write deadlines as a verbatim stream.
func (s *Server) pipeConverted(w http.ResponseWriter, r *http.Request, resp *http.Response, stages ...translate.Converter) (int64, error) {
	orig := resp.Body
	pr, pw := io.Pipe()
	go func() {
		err := runConverters(r.Context(), orig, pw, stages...)
		_ = orig.Close()
		_ = pw.CloseWithError(err)
	}()

	resp.Body = pr
	defer pr.Close()
	resp.Header.Set("Content-Type", "text/event-stream")

	contentType := "text/event-stream"
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	_ = http.NewResponseController(w).Flush()

	n, err := s.Upstream.Relay(w, resp, true)
	// Unblock the converter goroutine if Relay stopped early (client gone or a
	// stalled stream), otherwise it could sit in a Read on the upstream body.
	_ = orig.Close()
	return n, err
}

// runConverters chains converter stages through in-memory pipes.
func runConverters(ctx context.Context, src io.Reader, dst io.Writer, stages ...translate.Converter) error {
	switch len(stages) {
	case 0:
		return nil
	case 1:
		return stages[0](ctx, src, dst, nil)
	default:
		pr, pw := io.Pipe()
		go func() {
			err := stages[0](ctx, src, pw, nil)
			_ = pw.CloseWithError(err)
		}()
		err := stages[1](ctx, pr, dst, nil)
		// Do not wait for the first stage: it may be blocked in a Read on the
		// upstream body. Closing pr unblocks its next Write, and the caller
		// closes src on return.
		_ = pr.Close()
		return err
	}
}

// rawString returns a JSON string value, or "" when it is absent or not a string.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// writeError emits the error envelope the ingress dialect expects.
func (s *Server) writeError(w http.ResponseWriter, in ingress, status int, message, kind string) {
	if in == ingressAnthropic {
		writeAnthropicError(w, status, message, kind)
		return
	}
	writeJSONError(w, status, message, kind)
}

func (s *Server) badRequest(w http.ResponseWriter, r *http.Request, in ingress, err error) {
	LoggerFrom(r.Context()).Warn("bad request", "err", err)
	s.writeError(w, in, http.StatusBadRequest, err.Error(), "invalid_request_error")
}

func ingressName(in ingress) string {
	if in == ingressAnthropic {
		return "anthropic"
	}
	return "openai"
}

// translatable reports whether a provider format has a request encoder and a
// response decoder. Ollama's native /api/chat is still out of scope.
func translatable(format string) bool {
	switch format {
	case registry.FormatChat, registry.FormatAnthropic, registry.FormatGemini, registry.FormatResponses:
		return true
	}
	return false
}

// writeAnthropicError emits the error shape Anthropic clients parse. The
// OpenAI envelope would be silently misread as a successful message by some
// SDKs, so the ingress dialect decides the envelope.
func writeAnthropicError(w http.ResponseWriter, status int, message, kind string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    kind,
			"message": message,
		},
	})
}

// tokenSaverHeader reports the saver state on the response, which makes the
// feature observable from a client without reading the logs.
func tokenSaverHeader(req *generationRequest) string {
	if req == nil || !req.saving {
		return ""
	}
	if req.stats.Before == 0 {
		return "on; saved=0"
	}
	return fmt.Sprintf("on; saved=%d/%d", req.stats.Saved, req.stats.Before)
}

// rewriteModel replaces only the "model" member of a same-dialect body, so the
// client's exact bytes survive untouched whenever routing did not change it.
func rewriteModel(payload map[string]json.RawMessage, model string) ([]byte, error) {
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	payload["model"] = raw
	return json.Marshal(payload)
}
