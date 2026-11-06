package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lgllm/internal/config"
)

func okUpstream(t *testing.T, usage string) *config.Config {
	t.Helper()
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = ioString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]`+usage+`}`)
	})
	return cfg
}

func TestRateLimitRejectsBurst(t *testing.T) {
	cfg := okUpstream(t, "")
	cfg.Auth.RateLimit = config.RateLimit{RequestsPerMinute: 1, Burst: 1}
	s := newTestServer(t, cfg, nil)

	body := `{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`
	first := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body)
	}

	second := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("429 has no Retry-After header")
	}
	if !strings.Contains(second.Body.String(), "rate limit") {
		t.Errorf("body = %s", second.Body)
	}

	metrics := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), `lgllm_rate_limited_total{reason="rate"} 1`) {
		t.Errorf("metrics missing the rate-limit counter:\n%s", metrics.Body.String())
	}
}

func TestRateLimitDailyTokens(t *testing.T) {
	cfg := okUpstream(t, `,"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)
	cfg.Auth.RateLimit = config.RateLimit{RequestsPerMinute: 600, Burst: 600, TokensPerDay: 10}
	s := newTestServer(t, cfg, nil)

	body := `{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`
	if res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))); res.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", res.Code, res.Body)
	}

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "quota") {
		t.Errorf("body = %s, want a quota message", res.Body)
	}

	usage := do(s, httptest.NewRequest(http.MethodGet, "/usage?limit=1", nil))
	var payload struct {
		Entries []struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(usage.Body.Bytes(), &payload); err != nil {
		t.Fatalf("usage body = %s: %v", usage.Body, err)
	}
	if len(payload.Entries) != 1 || payload.Entries[0].PromptTokens != 10 {
		t.Errorf("usage = %+v", payload.Entries)
	}
}

func TestRateLimitAnthropicEnvelope(t *testing.T) {
	cfg := okUpstream(t, "")
	cfg.Providers[0].Format = "anthropic"
	cfg.Providers[0].Auth = "x-api-key"
	cfg.Auth.RateLimit = config.RateLimit{RequestsPerMinute: 1, Burst: 1}
	s := newTestServer(t, cfg, nil)

	// A pass-through Anthropic request needs a working upstream; the mock answers
	// OpenAI-shaped JSON, which is fine because the response is relayed verbatim.
	body := `{"model":"testprov/test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	if res := do(s, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))); res.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", res.Code, res.Body)
	}

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body = %s: %v", res.Body, err)
	}
	if envelope.Type != "error" || envelope.Error.Type != "rate_limit_error" {
		t.Errorf("envelope = %+v", envelope)
	}
}
