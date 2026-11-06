package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lgllm/internal/config"
)

func TestMetricsAndUsageAfterRequest(t *testing.T) {
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = ioString(w, `{
		  "id":"x","object":"chat.completion","model":"gpt-4o-mini",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`)
	})
	cfg.Providers[0].Models = []config.Model{{ID: "gpt-4o-mini"}}
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}

	metricsRes := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metricsRes.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", metricsRes.Code)
	}
	body := metricsRes.Body.String()
	for _, want := range []string{
		`lgllm_requests_total{ingress="openai",provider="testprov",model="gpt-4o-mini",status="200"} 1`,
		`lgllm_tokens_total{kind="prompt",provider="testprov",model="gpt-4o-mini"} 10`,
		`lgllm_tokens_total{kind="completion",provider="testprov",model="gpt-4o-mini"} 5`,
		`lgllm_request_duration_seconds_count{provider="testprov",model="gpt-4o-mini"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}

	usageRes := do(s, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if usageRes.Code != http.StatusOK {
		t.Fatalf("usage status = %d, body = %s", usageRes.Code, usageRes.Body)
	}
	var usage struct {
		Entries []struct {
			Ingress          string  `json:"ingress"`
			Requested        string  `json:"requested"`
			Provider         string  `json:"provider"`
			Model            string  `json:"model"`
			Status           int     `json:"status"`
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			CostUSD          float64 `json:"cost_usd"`
			CostKnown        bool    `json:"cost_known"`
			RequestID        string  `json:"request_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(usageRes.Body.Bytes(), &usage); err != nil {
		t.Fatalf("usage body = %s: %v", usageRes.Body, err)
	}
	if len(usage.Entries) != 1 {
		t.Fatalf("entries = %+v", usage.Entries)
	}
	e := usage.Entries[0]
	if e.Status != 200 || e.Provider != "testprov" || e.Model != "gpt-4o-mini" {
		t.Errorf("entry = %+v", e)
	}
	if e.PromptTokens != 10 || e.CompletionTokens != 5 {
		t.Errorf("tokens = %d/%d", e.PromptTokens, e.CompletionTokens)
	}
	if !e.CostKnown || e.CostUSD <= 0 {
		t.Errorf("cost = %v, known = %v", e.CostUSD, e.CostKnown)
	}
	if e.RequestID == "" {
		t.Error("the usage entry has no request id")
	}
}

func TestUsageRecordsFailuresAndAttemptMetrics(t *testing.T) {
	bad := jsonUpstream(t, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	bad2 := jsonUpstream(t, `{"error":{"message":"busy"}}`, http.StatusServiceUnavailable)
	cfg := testConfig(t, func(c *config.Config) {
		providerAt(c, "one", bad.URL, "sk-one")
		providerAt(c, "two", bad2.URL, "sk-two")
		c.Combos = []config.Combo{{
			Name:    "combo",
			Members: []string{"one/one-model", "two/two-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", res.Code)
	}

	metricsRes := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`lgllm_upstream_attempt_failures_total{provider="one",status="500"} 1`,
		`lgllm_upstream_attempt_failures_total{provider="two",status="503"} 1`,
		`status="502"} 1`,
	} {
		if !strings.Contains(metricsRes.Body.String(), want) {
			t.Errorf("missing %q in:\n%s", want, metricsRes.Body.String())
		}
	}

	usageRes := do(s, httptest.NewRequest(http.MethodGet, "/usage?limit=1", nil))
	var usage struct {
		Entries []struct {
			Status int    `json:"status"`
			Error  string `json:"error"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(usageRes.Body.Bytes(), &usage); err != nil {
		t.Fatalf("body = %s: %v", usageRes.Body, err)
	}
	if len(usage.Entries) != 1 {
		t.Fatalf("entries = %+v", usage.Entries)
	}
	if usage.Entries[0].Status != http.StatusBadGateway {
		t.Errorf("status = %d", usage.Entries[0].Status)
	}
	if !strings.Contains(usage.Entries[0].Error, "boom") || !strings.Contains(usage.Entries[0].Error, "busy") {
		t.Errorf("error summary = %q", usage.Entries[0].Error)
	}
}

func TestMetricsCanBeDisabled(t *testing.T) {
	s := newTestServer(t, testConfig(t, func(c *config.Config) {
		c.Metrics.Enabled = false
	}), nil)

	for _, path := range []string{"/metrics", "/usage"} {
		res := do(s, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, res.Code)
		}
	}
}

// ioString is a small helper so the tests read without importing io at every
// call site.
func ioString(w http.ResponseWriter, s string) (int, error) {
	return w.Write([]byte(s))
}
