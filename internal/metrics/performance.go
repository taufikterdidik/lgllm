package metrics

import (
	"fmt"
	"io"
	"sort"

	"lgllm/internal/perf"
)

// Performance gauges are rendered from the in-memory perf window at scrape
// time. They are gauges, not counters: the value is the average of the last N
// attempts, so it can go up or down.
const (
	ttftGaugeName     = "lgllm_model_ttft_seconds"
	tpsGaugeName      = "lgllm_model_tps"
	requestsGaugeName = "lgllm_model_requests_window"
)

// WritePerformanceGauges appends the per-model performance window to a
// Prometheus exposition. Models without samples still get a requests gauge (0),
// which makes "configured but unused" visible in dashboards.
func WritePerformanceGauges(w io.Writer, models []perf.Aggregate) error {
	models = append([]perf.Aggregate(nil), models...)
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].Model < models[j].Model
	})

	if _, err := fmt.Fprintf(w,
		"# HELP %s Attempts currently kept for a model.\n"+
			"# TYPE %s gauge\n",
		requestsGaugeName, requestsGaugeName); err != nil {
		return err
	}
	for _, a := range models {
		if _, err := fmt.Fprintf(w, "%s{provider=\"%s\",model=\"%s\"} %d\n",
			requestsGaugeName, esc(a.Provider), esc(a.Model), a.Attempts); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(w,
		"# HELP %s Average time to first content token over the recent window.\n"+
			"# TYPE %s gauge\n",
		ttftGaugeName, ttftGaugeName); err != nil {
		return err
	}
	for _, a := range models {
		for _, mode := range perfModes(a) {
			if _, err := fmt.Fprintf(w, "%s{provider=\"%s\",model=\"%s\",mode=\"%s\"} %g\n",
				ttftGaugeName, esc(a.Provider), esc(a.Model), mode.name,
				mode.stats.AvgTTFT.Seconds()); err != nil {
				return err
			}
		}
	}

	if _, err := fmt.Fprintf(w,
		"# HELP %s Average completion tokens per second over the recent window.\n"+
			"# TYPE %s gauge\n",
		tpsGaugeName, tpsGaugeName); err != nil {
		return err
	}
	for _, a := range models {
		for _, mode := range perfModes(a) {
			if !mode.stats.TPSKnown {
				continue
			}
			if _, err := fmt.Fprintf(w, "%s{provider=\"%s\",model=\"%s\",mode=\"%s\"} %g\n",
				tpsGaugeName, esc(a.Provider), esc(a.Model), mode.name,
				mode.stats.AvgTPS); err != nil {
				return err
			}
		}
	}
	return nil
}

type perfMode struct {
	name  string
	stats perf.ModeStats
}

// perfModes returns only the modes that have successful samples, so a model
// that has only ever produced JSON responses does not claim a stream TTFT.
func perfModes(a perf.Aggregate) []perfMode {
	out := make([]perfMode, 0, 2)
	if a.Stream.Samples > 0 {
		out = append(out, perfMode{name: "stream", stats: a.Stream})
	}
	if a.NonStream.Samples > 0 {
		out = append(out, perfMode{name: "non_stream", stats: a.NonStream})
	}
	return out
}
