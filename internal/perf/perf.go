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

// ModeStats is the average of successful samples in one mode.
type ModeStats struct {
	Samples    int
	AvgTTFT    time.Duration
	AvgLatency time.Duration
	AvgTPS     float64
	// TokensKnown reports whether at least one sample carried provider usage.
	TokensKnown bool
	// TPSKnown reports whether AvgTPS can be shown.
	TPSKnown bool
	// TPSEstimated marks a non-stream average, where first-token time does not
	// exist and TPS falls back to tokens/latency.
	TPSEstimated bool
}

// Aggregate is one model's window, split by mode because a streamed TPS and a
// whole-response TPS are not comparable.
type Aggregate struct {
	Provider  string
	Model     string
	Attempts  int
	Successes int
	Failures  int
	LastSeen  time.Time
	Stream    ModeStats
	NonStream ModeStats
}

type key struct {
	provider string
	model    string
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
	}
}

// WindowSize reports the configured number of attempts kept per model.
func (r *Registry) WindowSize() int {
	if r == nil {
		return DefaultWindow
	}
	return r.size
}

// Record adds one attempt. At is filled with the current time when zero, which
// lets callers omit it.
func (r *Registry) Record(provider, model string, s Sample) {
	if r == nil || provider == "" || model == "" {
		return
	}
	if s.At.IsZero() {
		s.At = time.Now()
	}
	k := key{provider: provider, model: model}
	r.mu.Lock()
	w := r.windows[k]
	if w == nil {
		w = &window{size: r.size}
		r.windows[k] = w
	}
	w.add(s)
	r.mu.Unlock()
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

	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
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
		m          ModeStats
		ttft, lat  float64
		tps        float64
		tpsSamples int
	)
	for _, s := range samples {
		if !s.OK || s.Stream != stream {
			continue
		}
		m.Samples++
		ttft += float64(s.TTFT)
		lat += float64(s.Latency)
		if s.TokensKnown {
			m.TokensKnown = true
		}
		if s.TPS > 0 {
			tps += s.TPS
			tpsSamples++
		}
		if s.TPSEstimated {
			m.TPSEstimated = true
		}
	}
	if m.Samples == 0 {
		return m
	}
	m.AvgTTFT = time.Duration(ttft / float64(m.Samples))
	m.AvgLatency = time.Duration(lat / float64(m.Samples))
	if tpsSamples > 0 {
		m.AvgTPS = tps / float64(tpsSamples)
		m.TPSKnown = true
	}
	return m
}
