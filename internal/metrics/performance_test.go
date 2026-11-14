package metrics

import (
	"strings"
	"testing"
	"time"

	"lgllm/internal/perf"
)

func TestWritePerformanceGauges(t *testing.T) {
	models := []perf.Aggregate{
		{
			Provider: "z", Model: "unused",
		},
		{
			Provider: "p", Model: "m", Attempts: 4,
			Stream: perf.ModeStats{
				Samples: 3, AvgTTFT: 250 * time.Millisecond,
				TPSKnown: true, AvgTPS: 42.5,
			},
			NonStream: perf.ModeStats{
				Samples: 1, AvgTTFT: 900 * time.Millisecond,
				TPSKnown: true, AvgTPS: 12, TPSEstimated: true,
			},
		},
	}

	var b strings.Builder
	if err := WritePerformanceGauges(&b, models); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	for _, want := range []string{
		"# TYPE lgllm_model_requests_window gauge",
		`lgllm_model_requests_window{provider="p",model="m"} 4`,
		`lgllm_model_requests_window{provider="z",model="unused"} 0`,
		`lgllm_model_ttft_seconds{provider="p",model="m",mode="stream"} 0.25`,
		`lgllm_model_ttft_seconds{provider="p",model="m",mode="non_stream"} 0.9`,
		`lgllm_model_tps{provider="p",model="m",mode="stream"} 42.5`,
		`lgllm_model_tps{provider="p",model="m",mode="non_stream"} 12`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Sorted by provider/model, and no ttft/tps sample for a model with no
	// successful attempt.
	if strings.Index(out, `provider="p"`) > strings.Index(out, `provider="z"`) {
		t.Errorf("gauges are not sorted:\n%s", out)
	}
	if strings.Contains(out, `lgllm_model_ttft_seconds{provider="z"`) {
		t.Errorf("unused model exposed a TTFT gauge:\n%s", out)
	}
}
