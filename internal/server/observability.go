package server

import (
	"net/http"
	"regexp"
	"strconv"
	"time"

	"lgllm/internal/metrics"
	"lgllm/internal/perf"
	"lgllm/internal/pricing"
	"lgllm/internal/usagelog"
)

// handleMetrics serves GET /metrics in the Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.Config().Metrics.Enabled {
		writeJSONError(w, http.StatusNotFound, "metrics are disabled", "not_found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	log := LoggerFrom(r.Context())
	if err := s.Metrics.Render(w); err != nil {
		log.Warn("rendering metrics failed", "err", err)
		return
	}
	if s.Perf != nil {
		if err := metrics.WritePerformanceGauges(w, s.Perf.Snapshot()); err != nil {
			log.Warn("rendering performance gauges failed", "err", err)
		}
	}
}

// handleUsage serves GET /usage, the recent request ring buffer.
//
// It is a debugging aid, not a billing ledger: entries are bounded and lost on
// restart. The admin API (phase 9) will read the same store.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.Config().Metrics.Enabled {
		writeJSONError(w, http.StatusNotFound, "usage log is disabled", "not_found")
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
	entries := s.Usage.Snapshot(limit)
	if entries == nil {
		entries = []usagelog.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":  "list",
		"entries": entries,
	})
}

// recordRequest stores one finished request in the metrics counters and the
// usage ring. It is called exactly once per request: on success right before the
// response is written, and on failure after the last attempt.
func (s *Server) recordRequest(req *generationRequest, provider, model string, status int,
	promptTokens, completionTokens int, errText string) {

	elapsed := time.Since(req.start)
	ingress := ingressName(req.in)
	s.Metrics.ObserveRequest(ingress, provider, model, status, elapsed)
	s.Metrics.AddTokens(provider, model, "prompt", promptTokens)
	s.Metrics.AddTokens(provider, model, "completion", completionTokens)
	if req.stats.Saved > 0 {
		s.Metrics.AddSavedBytes(provider, model, req.stats.Saved)
	}
	if s.RateLimit != nil {
		s.RateLimit.AddTokens(req.clientKey, promptTokens+completionTokens)
	}

	cost, known := pricing.EstimateFor(provider, model, promptTokens, completionTokens)
	s.Usage.Add(usagelog.Entry{
		Time:             req.start,
		RequestID:        req.requestID,
		Ingress:          ingress,
		Requested:        req.requested,
		Provider:         provider,
		Model:            model,
		Status:           status,
		Stream:           req.stream,
		ClientKey:        req.clientKey,
		DurationMS:       elapsed.Milliseconds(),
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		SavedBytes:       req.stats.Saved,
		CostUSD:          cost,
		CostKnown:        known,
		Error:            errText,
	})
	if status < 400 && req.combo != "" && req.comboMember != "" && s.comboStatus != nil {
		s.comboStatus.Used(req.combo, req.comboMember, time.Now())
	}
}

// recordStreamRequest commits the usage entry after the stream finished. The
// tap sees the provider's trailing usage chunk when it sends one, so streamed
// prompt/completion tokens and cost match the non-stream path instead of
// staying zero. A provider that never reports usage keeps zero tokens rather
// than an invented count.
func (s *Server) recordStreamRequest(req *generationRequest, provider, model string, tap *perf.SSETap) {
	var promptTokens, completionTokens int
	if tap != nil {
		if stats := tap.Stats(); stats.TokensKnown {
			promptTokens = stats.PromptTokens
			completionTokens = stats.CompletionTokens
		}
	}
	s.recordRequest(req, provider, model, http.StatusOK, promptTokens, completionTokens, "")
}

var (
	// Secrets that commonly appear in a pasted request body. This is a safety
	// net for debug logs, not a DLP product: never log a body at info level.
	secretValue = regexp.MustCompile(`(?i)("(?:api[_-]?key|authorization|token|secret|password)"\s*:\s*")[^"]*(")`)
	bearerValue = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]+`)
	skValue     = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`)
)

func redactBody(body []byte) string {
	const maxDebugBody = 2048
	s := string(body)
	if len(s) > maxDebugBody {
		s = s[:maxDebugBody] + "...[truncated]"
	}
	s = secretValue.ReplaceAllString(s, `$1***$2`)
	s = bearerValue.ReplaceAllString(s, `$1 ***`)
	s = skValue.ReplaceAllString(s, "sk-***")
	return s
}
