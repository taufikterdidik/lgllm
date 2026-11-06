// Package metrics keeps the counters exposed at GET /metrics.
//
// It implements the small subset of the Prometheus text format lgllm needs
// (counters, one gauge, one histogram) rather than pulling in a client library:
// the process is meant to stay a single dependency-light binary.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// durationBuckets covers a fast local model up to a long tool-using stream.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type requestKey struct {
	Ingress  string
	Provider string
	Model    string
	Status   int
}

type durationKey struct {
	Provider string
	Model    string
}

type durationStats struct {
	Count   int64
	Sum     float64
	Buckets []int64 // len(durationBuckets)
}

type tokenKey struct {
	Provider string
	Model    string
	Kind     string
}

type providerKey struct {
	Provider string
	Model    string
}

type failureKey struct {
	Provider string
	Status   int
}

// Metrics is safe for concurrent use.
type Metrics struct {
	mu          sync.Mutex
	requests    map[requestKey]int64
	durations   map[durationKey]*durationStats
	tokens      map[tokenKey]int64
	saved       map[providerKey]int64
	failures    map[failureKey]int64
	rateLimited map[string]int64
	inflight    int64
}

// New returns an empty collector.
func New() *Metrics {
	return &Metrics{
		requests:    make(map[requestKey]int64),
		durations:   make(map[durationKey]*durationStats),
		tokens:      make(map[tokenKey]int64),
		saved:       make(map[providerKey]int64),
		failures:    make(map[failureKey]int64),
		rateLimited: make(map[string]int64),
	}
}

// IncInflight and DecInflight track requests currently being served.
func (m *Metrics) IncInflight() {
	m.mu.Lock()
	m.inflight++
	m.mu.Unlock()
}

// DecInflight marks one request finished.
func (m *Metrics) DecInflight() {
	m.mu.Lock()
	if m.inflight > 0 {
		m.inflight--
	}
	m.mu.Unlock()
}

// ObserveRequest records one finished request.
func (m *Metrics) ObserveRequest(ingress, provider, model string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{ingress, provider, model, status}]++

	key := durationKey{provider, model}
	stats := m.durations[key]
	if stats == nil {
		stats = &durationStats{Buckets: make([]int64, len(durationBuckets))}
		m.durations[key] = stats
	}
	sec := d.Seconds()
	stats.Count++
	stats.Sum += sec
	for i, b := range durationBuckets {
		if sec <= b {
			stats.Buckets[i]++
		}
	}
}

// AddTokens counts prompt or completion tokens for a provider/model.
func (m *Metrics) AddTokens(provider, model, kind string, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.tokens[tokenKey{provider, model, kind}] += int64(n)
	m.mu.Unlock()
}

// AddSavedBytes counts bytes removed by the token saver.
func (m *Metrics) AddSavedBytes(provider, model string, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.saved[providerKey{provider, model}] += int64(n)
	m.mu.Unlock()
}

// ObserveFailure counts one failed upstream attempt. Unlike ObserveRequest this
// is per attempt, so it also records retries that were later recovered.
func (m *Metrics) ObserveFailure(provider string, status int) {
	m.mu.Lock()
	m.failures[failureKey{provider, status}]++
	m.mu.Unlock()
}

// ObserveRateLimited counts a request rejected before reaching a provider.
func (m *Metrics) ObserveRateLimited(reason string) {
	if reason == "" {
		reason = "unknown"
	}
	m.mu.Lock()
	m.rateLimited[reason]++
	m.mu.Unlock()
}

// Render writes the Prometheus text exposition.
func (m *Metrics) Render(w io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := io.WriteString(w, "# HELP lgllm_inflight_requests Requests currently being served.\n"+
		"# TYPE lgllm_inflight_requests gauge\n"+
		fmt.Sprintf("lgllm_inflight_requests %d\n", m.inflight)); err != nil {
		return err
	}

	if err := writeCounter(w,
		"lgllm_requests_total",
		"Requests by ingress, provider, model and final status.",
		func(emit func(labels string, value int64)) {
			for k, v := range m.requests {
				emit(fmt.Sprintf(`ingress="%s",provider="%s",model="%s",status="%d"`,
					esc(k.Ingress), esc(k.Provider), esc(k.Model), k.Status), v)
			}
		}); err != nil {
		return err
	}

	if err := writeHistogram(w, m.durations); err != nil {
		return err
	}

	if err := writeCounter(w,
		"lgllm_tokens_total",
		"Tokens seen in upstream responses.",
		func(emit func(labels string, value int64)) {
			for k, v := range m.tokens {
				emit(fmt.Sprintf(`kind="%s",provider="%s",model="%s"`,
					esc(k.Kind), esc(k.Provider), esc(k.Model)), v)
			}
		}); err != nil {
		return err
	}

	if err := writeCounter(w,
		"lgllm_token_saver_saved_bytes_total",
		"Bytes removed from tool results by the token saver.",
		func(emit func(labels string, value int64)) {
			for k, v := range m.saved {
				emit(fmt.Sprintf(`provider="%s",model="%s"`,
					esc(k.Provider), esc(k.Model)), v)
			}
		}); err != nil {
		return err
	}

	if err := writeCounter(w,
		"lgllm_rate_limited_total",
		"Requests rejected by the per-key rate limit or daily token quota.",
		func(emit func(labels string, value int64)) {
			for reason, v := range m.rateLimited {
				emit(fmt.Sprintf(`reason="%s"`, esc(reason)), v)
			}
		}); err != nil {
		return err
	}

	return writeCounter(w,
		"lgllm_upstream_attempt_failures_total",
		"Failed upstream attempts, including ones recovered by a retry.",
		func(emit func(labels string, value int64)) {
			for k, v := range m.failures {
				emit(fmt.Sprintf(`provider="%s",status="%d"`,
					esc(k.Provider), k.Status), v)
			}
		})
}

func writeCounter(w io.Writer, name, help string, each func(emit func(labels string, value int64))) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name); err != nil {
		return err
	}
	var lines []string
	each(func(labels string, value int64) {
		lines = append(lines, fmt.Sprintf("%s{%s} %d", name, labels, value))
	})
	sort.Strings(lines)
	for _, line := range lines {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func writeHistogram(w io.Writer, durations map[durationKey]*durationStats) error {
	const name = "lgllm_request_duration_seconds"
	if _, err := fmt.Fprintf(w, "# HELP %s Request duration by provider and model.\n# TYPE %s histogram\n", name, name); err != nil {
		return err
	}

	keys := make([]durationKey, 0, len(durations))
	for k := range durations {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Provider != keys[j].Provider {
			return keys[i].Provider < keys[j].Provider
		}
		return keys[i].Model < keys[j].Model
	})

	for _, k := range keys {
		stats := durations[k]
		base := fmt.Sprintf(`provider="%s",model="%s"`, esc(k.Provider), esc(k.Model))
		for i, b := range durationBuckets {
			if _, err := fmt.Fprintf(w, "%s_bucket{%s,le=\"%g\"} %d\n", name, base, b, stats.Buckets[i]); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, base, stats.Count); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_sum{%s} %g\n", name, base, stats.Sum); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_count{%s} %d\n", name, base, stats.Count); err != nil {
			return err
		}
	}
	return nil
}

// esc applies the Prometheus label escaping rules.
func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
