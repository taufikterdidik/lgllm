package server

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"lgllm/internal/perf"
	"lgllm/internal/translate"
)

// recordAttempt stores one upstream attempt in the per-model performance
// window. Failures keep their latency for the success rate but never enter the
// TTFT/latency/TPS averages.
func (s *Server) recordAttempt(provider, model string, sample perf.Sample) {
	if s.Perf == nil || provider == "" || model == "" {
		return
	}
	s.Perf.Record(provider, model, sample)
}

func (s *Server) recordFailedAttempt(provider, model string, stream bool, latency time.Duration) {
	s.recordAttempt(provider, model, perf.Sample{
		OK:      false,
		Stream:  stream,
		Latency: latency,
	})
}

// recordStreamAttempt commits a finished stream attempt. TPS uses the content
// window (first token to last token); with a single content event that window
// is zero, so it falls back to the whole latency and is marked estimated.
func (s *Server) recordStreamAttempt(provider, model string, start time.Time, latency time.Duration, tap *perf.SSETap) {
	sample := perf.Sample{
		OK:      true,
		Stream:  true,
		Latency: latency,
		TTFT:    latency,
	}
	if tap != nil {
		stats := tap.Stats()
		if !stats.FirstContent.IsZero() {
			sample.TTFT = stats.FirstContent.Sub(start)
		}
		sample.Tokens = stats.CompletionTokens
		sample.TokensKnown = stats.TokensKnown
		if stats.TokensKnown && stats.CompletionTokens > 0 {
			if span := stats.ContentDuration(); span > 0 {
				sample.TPS = float64(stats.CompletionTokens) / span.Seconds()
			} else if latency > 0 {
				sample.TPS = float64(stats.CompletionTokens) / latency.Seconds()
				sample.TPSEstimated = true
			}
		}
	}
	if sample.TTFT <= 0 {
		sample.TTFT = latency
	}
	s.recordAttempt(provider, model, sample)
}

// recordStreamResult decides whether a relay that already wrote bytes to the
// client counts as a successful attempt. A stream cut in the middle is stored
// as a failure with its partial latency, matching the success-rate definition.
func (s *Server) recordStreamResult(provider, model string, start time.Time, tap *perf.SSETap, relayErr error) {
	latency := time.Since(start)
	if relayErr != nil {
		s.recordFailedAttempt(provider, model, true, latency)
		return
	}
	s.recordStreamAttempt(provider, model, start, latency, tap)
}

// recordNonStreamAttempt commits a finished JSON response. There is no
// first-token signal, so TTFT equals latency and TPS is marked estimated.
func (s *Server) recordNonStreamAttempt(provider, model string, latency time.Duration, usage translate.Usage) {
	sample := perf.Sample{
		OK:           true,
		Stream:       false,
		Latency:      latency,
		TTFT:         latency,
		Tokens:       usage.CompletionTokens,
		TokensKnown:  usage.CompletionTokens > 0,
		TPSEstimated: true,
	}
	if sample.TokensKnown && latency > 0 {
		sample.TPS = float64(usage.CompletionTokens) / latency.Seconds()
	}
	s.recordAttempt(provider, model, sample)
}

// --- admin API ---------------------------------------------------------------

type perfModeDTO struct {
	Samples      int      `json:"samples"`
	AvgTTFTMS    *float64 `json:"avg_ttft_ms"`
	AvgLatencyMS *float64 `json:"avg_latency_ms"`
	AvgTPS       *float64 `json:"avg_tps"`
	TokensKnown  bool     `json:"tokens_known"`
	TPSEstimated bool     `json:"tps_estimated"`
}

type perfModelDTO struct {
	Provider  string      `json:"provider"`
	Model     string      `json:"model"`
	Available bool        `json:"available"`
	Attempts  int         `json:"attempts"`
	Successes int         `json:"successes"`
	Failures  int         `json:"failures"`
	LastSeen  *time.Time  `json:"last_seen"`
	Stream    perfModeDTO `json:"stream"`
	NonStream perfModeDTO `json:"non_stream"`
}

// handleAdminPerformance serves GET /api/performance.
//
// Every advertised model is returned, including one that has never served a
// request (attempts: 0, values null), so the page can answer "what do I have"
// as well as "how fast is it". Samples for models that are no longer in the
// registry are dropped.
func (s *Server) handleAdminPerformance(w http.ResponseWriter, r *http.Request) {
	window := perf.DefaultWindow
	byKey := make(map[string]perf.Aggregate)
	if s.Perf != nil {
		window = s.Perf.WindowSize()
		for _, a := range s.Perf.Snapshot() {
			byKey[a.Provider+"/"+a.Model] = a
		}
	}

	filter := strings.TrimSpace(r.URL.Query().Get("provider"))
	providers := s.Registry().Providers()
	data := make([]perfModelDTO, 0, len(providers))
	for _, p := range providers {
		if filter != "" && filter != p.ID && filter != p.Alias {
			continue
		}
		for _, m := range p.Models {
			data = append(data, buildPerfDTO(p.ID, m.ID, byKey[p.ID+"/"+m.ID]))
		}
	}
	sort.SliceStable(data, func(i, j int) bool {
		if data[i].Provider != data[j].Provider {
			return data[i].Provider < data[j].Provider
		}
		return data[i].Model < data[j].Model
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"window": window,
		"data":   data,
	})
}

func buildPerfDTO(provider, model string, a perf.Aggregate) perfModelDTO {
	dto := perfModelDTO{
		Provider:  provider,
		Model:     model,
		Available: true,
		Attempts:  a.Attempts,
		Successes: a.Successes,
		Failures:  a.Failures,
		Stream:    buildPerfModeDTO(a.Stream),
		NonStream: buildPerfModeDTO(a.NonStream),
	}
	if !a.LastSeen.IsZero() {
		seen := a.LastSeen.UTC()
		dto.LastSeen = &seen
	}
	return dto
}

func buildPerfModeDTO(m perf.ModeStats) perfModeDTO {
	dto := perfModeDTO{
		Samples:      m.Samples,
		TokensKnown:  m.TokensKnown,
		TPSEstimated: m.TPSEstimated,
	}
	if m.Samples > 0 {
		ttft := round1(float64(m.AvgTTFT) / float64(time.Millisecond))
		latency := round1(float64(m.AvgLatency) / float64(time.Millisecond))
		dto.AvgTTFTMS = &ttft
		dto.AvgLatencyMS = &latency
	}
	if m.TPSKnown {
		tps := round1(m.AvgTPS)
		dto.AvgTPS = &tps
	}
	return dto
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
