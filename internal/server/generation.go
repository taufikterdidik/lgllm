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

	target, err := reg.Lookup(requested)
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, registry.ErrNoDefaultProvider) {
			status = http.StatusBadRequest
		}
		log.Warn("model routing failed", "model", requested, "err", err)
		s.writeError(w, in, status, err.Error(), "invalid_request_error")
		return
	}
	provider, model := target.Provider, target.Model
	log.Debug("routed request",
		"requested", requested,
		"provider", provider.ID,
		"model", model,
		"alias", target.Alias,
		"combo", target.Combo,
		"ingress", ingressName(in),
	)

	conn, err := provider.Connection()
	if err != nil {
		log.Error("provider has no usable connection", "provider", provider.ID, "err", err)
		s.writeError(w, in, http.StatusBadGateway, err.Error(), "provider_unavailable")
		return
	}
	base, err := provider.Endpoint(conn)
	if err != nil {
		log.Error("provider endpoint is invalid", "provider", provider.ID, "err", err)
		s.writeError(w, in, http.StatusBadGateway, err.Error(), "provider_misconfigured")
		return
	}

	if !translatable(provider.Format) {
		s.writeError(w, in, http.StatusNotImplemented,
			fmt.Sprintf("provider %q speaks the %q format, which has no translator yet",
				provider.ID, provider.Format),
			"not_implemented")
		return
	}
	outbound, err := buildOutbound(in, provider.Format, body, payload, model, requested, stream)
	if err != nil {
		s.badRequest(w, r, in, err)
		return
	}

	endpoint, err := generationURL(provider.Format, base, model, stream)
	if err != nil {
		s.writeError(w, in, http.StatusBadGateway, err.Error(), "provider_misconfigured")
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(outbound))
	if err != nil {
		log.Error("cannot build upstream request", "url", endpoint, "err", err)
		s.writeError(w, in, http.StatusInternalServerError, "invalid upstream endpoint", "internal_error")
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
	for _, h := range []string{"User-Agent", "HTTP-Referer", "X-Title", "Anthropic-Beta"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	for k, v := range provider.Headers {
		req.Header.Set(k, v)
	}
	if provider.Format == registry.FormatAnthropic && req.Header.Get("anthropic-version") == "" {
		// The Messages API rejects a request without this header. A custom
		// provider definition often forgets it; the catalog entry carries it.
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	applyProviderAuth(req, provider, conn)

	start := time.Now()
	resp, err := s.Upstream.Do(req, provider.Timeout)
	if err != nil {
		if r.Context().Err() != nil {
			log.Info("client cancelled before the upstream answered",
				"provider", provider.ID, "model", model)
			return
		}
		status := upstream.TransportErrorStatus(err)
		log.Warn("upstream request failed",
			"provider", provider.ID, "model", model, "status", status, "err", err)
		s.writeError(w, in, status, "upstream request failed: "+err.Error(), "upstream_error")
		return
	}
	origBody := resp.Body
	defer origBody.Close()

	w.Header().Set("X-Lgllm-Provider", provider.ID)
	w.Header().Set("X-Lgllm-Model", model)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if in == ingressAnthropic {
			s.relayAsAnthropicError(w, resp)
		} else {
			relayUpstreamError(w, resp)
		}
		log.Warn("upstream returned an error",
			"provider", provider.ID, "model", model, "status", resp.StatusCode,
			"dur_ms", time.Since(start).Milliseconds())
		return
	}

	// Forward untouched only when both ends speak the same dialect.
	sameDialect := (in == ingressChat && provider.Format == registry.FormatChat) ||
		(in == ingressAnthropic && provider.Format == registry.FormatAnthropic)
	needsTranslation := !sameDialect

	if !needsTranslation {
		n, relayErr := s.relayVerbatim(w, resp)
		logRelay(log, provider, model, start, n, relayErr)
		return
	}

	if stream {
		stages := []translate.Converter{}
		if provider.Format != registry.FormatChat {
			conv, ok := translate.NewStreamConverter(provider.Format, translate.StreamOptions{Model: model})
			if !ok {
				s.writeError(w, in, http.StatusNotImplemented,
					"stream translation is not available for this provider format", "not_implemented")
				return
			}
			stages = append(stages, conv)
		}
		if in == ingressAnthropic {
			stages = append(stages, translate.AnthropicIngressConverter(model))
		}
		n, relayErr := s.pipeConverted(w, r, resp, stages...)
		logRelay(log, provider, model, start, n, relayErr)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(origBody, maxTranslatedBodyBytes))
	if err != nil {
		s.writeError(w, in, http.StatusBadGateway, "reading upstream response: "+err.Error(), "upstream_error")
		return
	}
	pivot, err := translate.DecodeResponse(provider.Format, raw)
	if err != nil {
		s.writeError(w, in, http.StatusBadGateway, "decoding upstream response: "+err.Error(), "upstream_error")
		return
	}
	var out []byte
	if in == ingressAnthropic {
		out, err = translate.EncodeAnthropicResponse(pivot)
	} else {
		out, err = translate.EncodeOpenAI(pivot)
	}
	if err != nil {
		s.writeError(w, in, http.StatusBadGateway, "encoding response: "+err.Error(), "upstream_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(out); err != nil {
		log.Debug("client stopped reading the response", "err", err)
	}
	log.Info("proxied chat completion",
		"provider", provider.ID,
		"model", model,
		"stream", false,
		"status", http.StatusOK,
		"bytes", len(out),
		"dur_ms", time.Since(start).Milliseconds(),
	)
}

// buildOutbound renders the upstream request body.
//
// For a provider that speaks the same dialect as the client, the bytes are
// forwarded (with only the model id rewritten when routing changed it). Anything
// else is decoded into the pivot and re-encoded for the provider.
func buildOutbound(in ingress, format string, body []byte, payload map[string]json.RawMessage, model, requested string, stream bool) ([]byte, error) {
	sameDialect := (in == ingressChat && format == registry.FormatChat) ||
		(in == ingressAnthropic && format == registry.FormatAnthropic)
	if sameDialect {
		if model == requested {
			return body, nil // keep the client's bytes exactly
		}
		return rewriteModel(payload, model)
	}

	var (
		pivot *translate.Request
		err   error
	)
	switch in {
	case ingressAnthropic:
		pivot, err = translate.DecodeAnthropic(body)
	default:
		pivot, err = translate.DecodeOpenAI(body)
	}
	if err != nil {
		return nil, err
	}
	pivot.Model = model
	pivot.Stream = stream
	return translate.EncodeRequest(format, pivot)
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
func logRelay(log *slog.Logger, provider *registry.Provider, model string, start time.Time, n int64, err error) {
	attrs := []any{
		"provider", provider.ID,
		"model", model,
		"bytes", n,
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

// relayAsAnthropicError rewrites a provider error into the Anthropic envelope,
// keeping the status and retry hint so a client can still back off.
func (s *Server) relayAsAnthropicError(w http.ResponseWriter, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if retry := resp.Header.Get("Retry-After"); retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	kind := "api_error"
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		kind = "invalid_request_error"
	}
	message := translate.ParseError(body)
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	writeAnthropicError(w, resp.StatusCode, message, kind)
}
