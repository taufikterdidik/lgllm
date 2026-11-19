package server

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"lgllm/internal/perf"
	"lgllm/internal/pricing"
	"lgllm/internal/translate"
)

// recordAttempt stores one upstream attempt in the per-model performance
// window and, when a connection id is known, in that credential's window too.
// Failures keep their latency for the success rate but never enter the
// TTFT/latency/TPS averages.
func (s *Server) recordAttempt(provider, model, connection string, sample perf.Sample) {
	if s.Perf == nil || provider == "" || model == "" {
		return
	}
	s.Perf.RecordConnection(provider, model, connection, sample)
}

func (s *Server) recordFailedAttempt(provider, model, connection string, stream bool, latency time.Duration) {
	s.recordAttempt(provider, model, connection, perf.Sample{
		OK:      false,
		Stream:  stream,
		Latency: latency,
	})
}

// recordStreamAttempt commits a finished stream attempt. TPS uses the content
// window (first token to last token); with a single content event that window
// is zero, so it falls back to the whole latency and is marked estimated.
func (s *Server) recordStreamAttempt(provider, model, connection string, start time.Time, latency time.Duration, tap *perf.SSETap) {
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
	s.recordAttempt(provider, model, connection, sample)
}

// recordStreamResult decides whether a relay that already wrote bytes to the
// client counts as a successful attempt. A stream cut in the middle is stored
// as a failure with its partial latency, matching the success-rate definition,
// unless the caller disappeared: a client abort is not a model failure. If the
// provider had already sent a finish reason, the response did complete from the
// model's side, so it is recorded as a success even though the trailing usage
// chunk may never have been read.
func (s *Server) recordStreamResult(ctx context.Context, provider, model, connection string, start time.Time, tap *perf.SSETap, relayErr error) {
	latency := time.Since(start)
	if relayErr != nil {
		if streamClientGone(ctx, relayErr) {
			if tap != nil && tap.Stats().SawFinish() {
				s.recordStreamAttempt(provider, model, connection, start, latency, tap)
			}
			return
		}
		s.recordFailedAttempt(provider, model, connection, true, latency)
		return
	}
	s.recordStreamAttempt(provider, model, connection, start, latency, tap)
}

// recordNonStreamAttempt commits a finished JSON response. There is no
// first-token signal, so TTFT equals latency and TPS is marked estimated.
func (s *Server) recordNonStreamAttempt(provider, model, connection string, latency time.Duration, usage translate.Usage) {
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
	s.recordAttempt(provider, model, connection, sample)
}

// --- admin API ---------------------------------------------------------------

type perfModeDTO struct {
	Samples      int      `json:"samples"`
	AvgTTFTMS    *float64 `json:"avg_ttft_ms"`
	AvgLatencyMS *float64 `json:"avg_latency_ms"`
	AvgTPS       *float64 `json:"avg_tps"`
	P50TTFTMS    *float64 `json:"p50_ttft_ms"`
	P50LatencyMS *float64 `json:"p50_latency_ms"`
	P50TPS       *float64 `json:"p50_tps"`
	TokensKnown  bool     `json:"tokens_known"`
	TPSEstimated bool     `json:"tps_estimated"`
	// LowSample warns that the average comes from fewer than
	// perf.LowSampleThreshold successes.
	LowSample bool `json:"low_sample"`
}

type perfConnectionDTO struct {
	Connection string      `json:"connection"`
	Attempts   int         `json:"attempts"`
	Successes  int         `json:"successes"`
	Failures   int         `json:"failures"`
	LastSeen   *time.Time  `json:"last_seen"`
	Stream     perfModeDTO `json:"stream"`
	NonStream  perfModeDTO `json:"non_stream"`
}

// perfSampleDTO is one point in the time-series view. Failures are included so
// the chart can mark them; their TTFT/TPS are null because no content arrived.
type perfSampleDTO struct {
	At           time.Time `json:"at"`
	OK           bool      `json:"ok"`
	Stream       bool      `json:"stream"`
	TTFTMS       *float64  `json:"ttft_ms"`
	LatencyMS    float64   `json:"latency_ms"`
	TPS          *float64  `json:"tps"`
	TPSEstimated bool      `json:"tps_estimated,omitempty"`
	Tokens       int       `json:"tokens,omitempty"`
	TokensKnown  bool      `json:"tokens_known"`
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
	// Pricing is the published list price per one million tokens. Known is
	// false for custom providers and models missing from the pricing table.
	Pricing perfPricingDTO `json:"pricing"`
	// Connections is the per-credential breakdown for this model. It is empty
	// when only model-level samples exist.
	Connections []perfConnectionDTO `json:"connections,omitempty"`
	// Series is present only with ?series=1, keeping the default payload small.
	Series []perfSampleDTO `json:"series,omitempty"`
}

type perfPricingDTO struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write,omitempty"`
	Known      bool    `json:"known"`
}

// handleAdminPerformance serves GET /api/performance.
//
// Every advertised model is returned, including one that has never served a
// request (attempts: 0, values null), so the page can answer "what do I have"
// as well as "how fast is it". Samples for models that are no longer in the
// registry are dropped. Add ?series=1 for the ring samples used by the charts.
func (s *Server) handleAdminPerformance(w http.ResponseWriter, r *http.Request) {
	window := perf.DefaultWindow
	byKey := make(map[string]perf.Aggregate)
	connByKey := make(map[string][]perf.Aggregate)
	if s.Perf != nil {
		window = s.Perf.WindowSize()
		for _, a := range s.Perf.Snapshot() {
			byKey[a.Provider+"/"+a.Model] = a
		}
		for _, a := range s.Perf.SnapshotConnections() {
			k := a.Provider + "/" + a.Model
			connByKey[k] = append(connByKey[k], a)
		}
	}

	wantSeries := r.URL.Query().Get("series") == "1"
	filter := strings.TrimSpace(r.URL.Query().Get("provider"))
	providers := s.Registry().Providers()
	data := make([]perfModelDTO, 0, len(providers))
	for _, p := range providers {
		if filter != "" && filter != p.ID && filter != p.Alias {
			continue
		}
		for _, m := range p.Models {
			k := p.ID + "/" + m.ID
			dto := buildPerfDTO(p.ID, m.ID, byKey[k])
			for _, c := range connByKey[k] {
				dto.Connections = append(dto.Connections, buildPerfConnectionDTO(c))
			}
			if wantSeries && s.Perf != nil {
				dto.Series = buildPerfSeries(s.Perf.Series(p.ID, m.ID))
			}
			data = append(data, dto)
		}
	}
	sort.SliceStable(data, func(i, j int) bool {
		if data[i].Provider != data[j].Provider {
			return data[i].Provider < data[j].Provider
		}
		return data[i].Model < data[j].Model
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"object":        "list",
		"window":        window,
		"low_sample_at": perf.LowSampleThreshold,
		"data":          data,
	})
}

func buildPerfDTO(provider, model string, a perf.Aggregate) perfModelDTO {
	rate, priced := pricing.ModelRateFor(provider, model)
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
	dto.Pricing = perfPricingDTO{
		Input:      rate.Input,
		Output:     rate.Output,
		CacheRead:  rate.CacheRead,
		CacheWrite: rate.CacheWrite,
		Known:      priced,
	}
	return dto
}

func buildPerfConnectionDTO(a perf.Aggregate) perfConnectionDTO {
	dto := perfConnectionDTO{
		Connection: a.Connection,
		Attempts:   a.Attempts,
		Successes:  a.Successes,
		Failures:   a.Failures,
		Stream:     buildPerfModeDTO(a.Stream),
		NonStream:  buildPerfModeDTO(a.NonStream),
	}
	if !a.LastSeen.IsZero() {
		seen := a.LastSeen.UTC()
		dto.LastSeen = &seen
	}
	return dto
}

func buildPerfSeries(samples []perf.Sample) []perfSampleDTO {
	if len(samples) == 0 {
		return nil
	}
	out := make([]perfSampleDTO, 0, len(samples))
	for _, s := range samples {
		dto := perfSampleDTO{
			At:           s.At.UTC(),
			OK:           s.OK,
			Stream:       s.Stream,
			LatencyMS:    round1(float64(s.Latency) / float64(time.Millisecond)),
			TPSEstimated: s.TPSEstimated,
			Tokens:       s.Tokens,
			TokensKnown:  s.TokensKnown,
		}
		if s.TTFT > 0 {
			ttft := round1(float64(s.TTFT) / float64(time.Millisecond))
			dto.TTFTMS = &ttft
		}
		if s.TPS > 0 {
			tps := round1(s.TPS)
			dto.TPS = &tps
		}
		out = append(out, dto)
	}
	return out
}

func buildPerfModeDTO(m perf.ModeStats) perfModeDTO {
	dto := perfModeDTO{
		Samples:      m.Samples,
		TokensKnown:  m.TokensKnown,
		TPSEstimated: m.TPSEstimated,
		LowSample:    m.Samples > 0 && m.Samples < perf.LowSampleThreshold,
	}
	if m.Samples > 0 {
		ttft := round1(float64(m.AvgTTFT) / float64(time.Millisecond))
		latency := round1(float64(m.AvgLatency) / float64(time.Millisecond))
		p50ttft := round1(float64(m.P50TTFT) / float64(time.Millisecond))
		p50latency := round1(float64(m.P50Latency) / float64(time.Millisecond))
		dto.AvgTTFTMS = &ttft
		dto.AvgLatencyMS = &latency
		dto.P50TTFTMS = &p50ttft
		dto.P50LatencyMS = &p50latency
	}
	if m.TPSKnown {
		tps := round1(m.AvgTPS)
		p50 := round1(m.P50TPS)
		dto.AvgTPS = &tps
		dto.P50TPS = &p50
	}
	return dto
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
