// Package perf keeps a small rolling window of real upstream attempts per
// provider/model so the admin UI can show recent TTFT, latency and TPS.
//
// It is deliberately in-memory: the numbers describe the last N attempts, not a
// billing ledger, and are lost on restart like the usage ring. Only timings and
// token counts are stored; prompts and responses never reach this package.
package perf

import (
	"sort"
	"sync"
	"time"
)

// DefaultWindow is the number of attempts kept per provider/model when the
// config does not say otherwise.
const DefaultWindow = 10

// LowSampleThreshold is the number of samples below which an average is shown
// with a "low sample" warning. Three is enough to make a wild first request
// visible without hiding early numbers entirely.
const LowSampleThreshold = 3

// Sample is one finished attempt against one provider/model.
//
// A failed attempt keeps its latency so the success rate is honest, but it is
// excluded from the TTFT/latency/TPS averages.
type Sample struct {
	At          time.Time
	OK          bool
	Stream      bool
	TTFT        time.Duration
	Latency     time.Duration
	Tokens      int
	TokensKnown bool
	// TPS is completion tokens divided by the content window (streams) or by
	// the whole latency (non-streams). TPSEstimated marks the latter.
	TPS          float64
	TPSEstimated bool
}

// ModeStats summarizes successful samples in one mode. Averages and medians
// are computed from the same window; the dashboard uses p50 while Prometheus
// keeps the average for aggregate monitoring.
type ModeStats struct {
	Samples    int
	AvgTTFT    time.Duration
	AvgLatency time.Duration
	AvgTPS     float64
	P50TTFT    time.Duration
	P50Latency time.Duration
	P50TPS     float64
	// TokensKnown reports whether at least one sample carried provider usage.
	TokensKnown bool
	// TPSKnown reports whether AvgTPS/P50TPS can be shown.
	TPSKnown bool
	// TPSEstimated marks a non-stream average, where first-token time does not
	// exist and TPS falls back to tokens/latency.
	TPSEstimated bool
}

// Aggregate is one model's window, split by mode because a streamed TPS and a
// whole-response TPS are not comparable.
type Aggregate struct {
	Provider string
	Model    string
	// Connection is set only by SnapshotConnections; the model-level aggregate
	// leaves it empty.
	Connection string
	Attempts   int
	Successes  int
	Failures   int
	LastSeen   time.Time
	Stream     ModeStats
	NonStream  ModeStats
}

type key struct {
	provider string
	model    string
}

type connKey struct {
	provider   string
	model      string
	connection string
}

type window struct {
	size    int
	samples []Sample
	next    int
}

func (w *window) add(s Sample) {
	if len(w.samples) < w.size {
		w.samples = append(w.samples, s)
		return
	}
	w.samples[w.next] = s
	w.next = (w.next + 1) % w.size
}

// snapshot returns the samples oldest-first, which makes tests and the average
// independent of the ring's internal layout.
func (w *window) snapshot() []Sample {
	if len(w.samples) < w.size {
		return append([]Sample(nil), w.samples...)
	}
	out := make([]Sample, 0, w.size)
	out = append(out, w.samples[w.next:]...)
	out = append(out, w.samples[:w.next]...)
	return out
}

// Registry is safe for concurrent use by request handlers.
type Registry struct {
	mu      sync.Mutex
	size    int
	windows map[key]*window
	// conns keeps a second ring per credential. The model-level window stays
	// the authoritative "last N attempts" for the main table; the connection
	// rings let the detail view separate one slow account from another.
	conns map[connKey]*window
	// store is optional. When set, every new sample is also queued for bbolt
	// persistence; the rings above remain the read path.
	store *Store
}

// New returns a registry keeping window samples per provider/model. A
// non-positive window falls back to DefaultWindow.
func New(windowSize int) *Registry {
	if windowSize <= 0 {
		windowSize = DefaultWindow
	}
	return &Registry{
		size:    windowSize,
		windows: make(map[key]*window),
		conns:   make(map[connKey]*window),
	}
}

// NewWithStore returns a registry backed by bbolt. The latest ring-sized
// samples for every key are loaded before returning, so the dashboard keeps its
// history across a restart. The caller owns Close.
func NewWithStore(windowSize int, store *Store) (*Registry, error) {
	r := New(windowSize)
	r.store = store
	if store == nil {
		return r, nil
	}
	samples, err := store.LoadRecent(r.size)
	if err != nil {
		return nil, err
	}
	for _, record := range samples {
		r.addRings(record.Provider, record.Model, record.Connection, record.Sample)
	}
	return r, nil
}

// Close drains and closes the optional persistence store.
func (r *Registry) Close() error {
	if r == nil || r.store == nil {
		return nil
	}
	return r.store.Close()
}

// WindowSize reports the configured number of attempts kept per model.
func (r *Registry) WindowSize() int {
	if r == nil {
		return DefaultWindow
	}
	return r.size
}

// Record adds one attempt without a connection label. It is kept for callers
// that only care about the model aggregate (and for tests).
func (r *Registry) Record(provider, model string, s Sample) {
	r.RecordConnection(provider, model, "", s)
}

// RecordConnection adds one attempt and, when connection is non-empty, also
// updates that credential's ring. At is filled with the current time when zero,
// which lets callers omit it.
func (r *Registry) RecordConnection(provider, model, connection string, s Sample) {
	if r == nil || provider == "" || model == "" {
		return
	}
	if s.At.IsZero() {
		s.At = time.Now()
	}
	r.mu.Lock()
	r.addRingsLocked(provider, model, connection, s)
	r.mu.Unlock()
	if r.store != nil {
		r.store.Enqueue(provider, model, connection, s)
	}
}

// addRings is used when loading persisted samples: it updates the read path
// without enqueueing the same row back to the database.
func (r *Registry) addRings(provider, model, connection string, sample Sample) {
	if r == nil || provider == "" || model == "" {
		return
	}
	r.mu.Lock()
	r.addRingsLocked(provider, model, connection, sample)
	r.mu.Unlock()
}

func (r *Registry) addRingsLocked(provider, model, connection string, sample Sample) {
	k := key{provider: provider, model: model}
	w := r.windows[k]
	if w == nil {
		w = &window{size: r.size}
		r.windows[k] = w
	}
	w.add(sample)
	if connection != "" {
		ck := connKey{provider: provider, model: model, connection: connection}
		cw := r.conns[ck]
		if cw == nil {
			cw = &window{size: r.size}
			r.conns[ck] = cw
		}
		cw.add(sample)
	}
}

// Series returns the model's samples oldest-first, including failures. It is
// used by the admin API when the UI asks for a time-series chart.
func (r *Registry) Series(provider, model string) []Sample {
	if r == nil || provider == "" || model == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.windows[key{provider: provider, model: model}]
	if w == nil {
		return nil
	}
	return w.snapshot()
}

// Snapshot returns every model's aggregate, sorted by provider then model.
func (r *Registry) Snapshot() []Aggregate {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	out := make([]Aggregate, 0, len(r.windows))
	for k, w := range r.windows {
		out = append(out, aggregate(k, w.snapshot()))
	}
	r.mu.Unlock()

	sortAggregates(out)
	return out
}

// SnapshotConnections returns one aggregate per provider/model/connection,
// sorted by provider, model then connection. A caller that recorded only with
// Record gets no connection rows, because no credential label was available.
func (r *Registry) SnapshotConnections() []Aggregate {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	out := make([]Aggregate, 0, len(r.conns))
	for ck, w := range r.conns {
		a := aggregate(key{provider: ck.provider, model: ck.model}, w.snapshot())
		a.Connection = ck.connection
		out = append(out, a)
	}
	r.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Connection < out[j].Connection
	})
	return out
}

func sortAggregates(out []Aggregate) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
}

func aggregate(k key, samples []Sample) Aggregate {
	a := Aggregate{Provider: k.provider, Model: k.model}
	for _, s := range samples {
		a.Attempts++
		if s.At.After(a.LastSeen) {
			a.LastSeen = s.At
		}
		if !s.OK {
			a.Failures++
			continue
		}
		a.Successes++
	}
	a.Stream = summarize(samples, true)
	a.NonStream = summarize(samples, false)
	return a
}

func summarize(samples []Sample, stream bool) ModeStats {
	var (
		m                       ModeStats
		totalTTFT, totalLatency float64
		totalTPS                float64
		ttfts                   []time.Duration
		latencies               []time.Duration
		tpsValues               []float64
	)
	for _, s := range samples {
		if !s.OK || s.Stream != stream {
			continue
		}
		m.Samples++
		totalTTFT += float64(s.TTFT)
		totalLatency += float64(s.Latency)
		ttfts = append(ttfts, s.TTFT)
		latencies = append(latencies, s.Latency)
		if s.TokensKnown {
			m.TokensKnown = true
		}
		if s.TPS > 0 {
			totalTPS += s.TPS
			tpsValues = append(tpsValues, s.TPS)
		}
		if s.TPSEstimated {
			m.TPSEstimated = true
		}
	}
	if m.Samples == 0 {
		return m
	}
	m.AvgTTFT = time.Duration(totalTTFT / float64(m.Samples))
	m.AvgLatency = time.Duration(totalLatency / float64(m.Samples))
	m.P50TTFT = medianDuration(ttfts)
	m.P50Latency = medianDuration(latencies)
	if len(tpsValues) > 0 {
		m.AvgTPS = totalTPS / float64(len(tpsValues))
		m.P50TPS = medianFloat(tpsValues)
		m.TPSKnown = true
	}
	return m
}

func medianDuration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
