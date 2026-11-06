package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestRenderCountersAndHistogram(t *testing.T) {
	m := New()
	m.IncInflight()
	m.ObserveRequest("openai", "ds", "deepseek-chat", 200, 120*time.Millisecond)
	m.ObserveRequest("openai", "ds", "deepseek-chat", 200, 80*time.Millisecond)
	m.ObserveRequest("anthropic", "ds", `model"with\quotes`, 502, 2*time.Second)
	m.AddTokens("ds", "deepseek-chat", "prompt", 100)
	m.AddTokens("ds", "deepseek-chat", "completion", 20)
	m.AddSavedBytes("ds", "deepseek-chat", 4096)
	m.ObserveFailure("ds", 429)

	var b strings.Builder
	if err := m.Render(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	for _, want := range []string{
		"lgllm_inflight_requests 1",
		`lgllm_requests_total{ingress="openai",provider="ds",model="deepseek-chat",status="200"} 2`,
		`lgllm_requests_total{ingress="anthropic",provider="ds",model="model\"with\\quotes",status="502"} 1`,
		`lgllm_request_duration_seconds_bucket{provider="ds",model="deepseek-chat",le="0.25"} 2`,
		`lgllm_request_duration_seconds_bucket{provider="ds",model="deepseek-chat",le="+Inf"} 2`,
		`lgllm_request_duration_seconds_count{provider="ds",model="deepseek-chat"} 2`,
		`lgllm_tokens_total{kind="prompt",provider="ds",model="deepseek-chat"} 100`,
		`lgllm_token_saver_saved_bytes_total{provider="ds",model="deepseek-chat"} 4096`,
		`lgllm_upstream_attempt_failures_total{provider="ds",status="429"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	m.DecInflight()
	b.Reset()
	_ = m.Render(&b)
	if !strings.Contains(b.String(), "lgllm_inflight_requests 0") {
		t.Errorf("inflight gauge did not decrement:\n%s", b.String())
	}
}

func TestRenderIsSorted(t *testing.T) {
	m := New()
	m.ObserveRequest("openai", "z", "m", 200, time.Millisecond)
	m.ObserveRequest("openai", "a", "m", 200, time.Millisecond)

	var b strings.Builder
	_ = m.Render(&b)
	first := strings.Index(b.String(), `provider="a"`)
	second := strings.Index(b.String(), `provider="z"`)
	if first < 0 || second < 0 || first > second {
		t.Errorf("counters are not sorted by labels:\n%s", b.String())
	}
}
