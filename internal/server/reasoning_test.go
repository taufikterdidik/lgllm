package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lgllm/internal/config"
	"lgllm/internal/registry"
)

func TestApplyReasoningEffortChatPreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"m","reasoning_effort":"low","temperature":0.2,"metadata":{"x":1}}`)
	out, err := applyReasoningEffort(body, &registry.Provider{ID: "custom", Format: registry.FormatChat}, "high")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", doc["reasoning_effort"])
	}
	if doc["temperature"] != 0.2 || doc["metadata"] == nil {
		t.Errorf("other fields were lost: %s", out)
	}
}

func TestApplyReasoningEffortResponsesPreservesSummary(t *testing.T) {
	body := []byte(`{"model":"m","reasoning":{"effort":"low","summary":"auto"},"input":[]}`)
	out, err := applyReasoningEffort(body, &registry.Provider{ID: "custom", Format: registry.FormatResponses}, "off")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Reasoning map[string]string `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Reasoning["effort"] != "off" || doc.Reasoning["summary"] != "auto" {
		t.Errorf("reasoning = %+v", doc.Reasoning)
	}
}

func TestApplyReasoningEffortNormalizesOffPerProvider(t *testing.T) {
	tests := []struct {
		provider string
		effort   string
		want     string
	}{
		{provider: "opencode-go", effort: "off", want: "none"},
		{provider: "opencode-go", effort: "none", want: "none"},
		{provider: "commandcode", effort: "none", want: "off"},
		{provider: "commandcode", effort: "off", want: "off"},
		{provider: "custom", effort: "off", want: "off"},
		{provider: "custom", effort: "none", want: "none"},
	}
	for _, tc := range tests {
		got := providerReasoningEffort(&registry.Provider{ID: tc.provider}, tc.effort)
		if got != tc.want {
			t.Errorf("%s %q = %q, want %q", tc.provider, tc.effort, got, tc.want)
		}
	}
}

func TestApplyReasoningEffortLeavesOtherFormatsAlone(t *testing.T) {
	body := []byte(`{"model":"m","thinking":{"type":"enabled"}}`)
	out, err := applyReasoningEffort(body, &registry.Provider{ID: "opencode-go-anthropic", Format: registry.FormatAnthropic}, "high")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("anthropic body changed: %s", out)
	}
}

func TestComboOffEffortUsesProviderSpelling(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		chosen   string
		want     string
	}{
		{name: "opencode off becomes none", provider: "opencode-go", chosen: "off", want: "none"},
		{name: "opencode none stays none", provider: "opencode-go", chosen: "none", want: "none"},
		{name: "commandcode off stays off", provider: "commandcode", chosen: "off", want: "off"},
		{name: "commandcode none becomes off", provider: "commandcode", chosen: "none", want: "off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			t.Cleanup(srv.Close)

			cfg := testConfig(t, func(c *config.Config) {
				c.Providers = []config.Provider{{
					ID: tc.provider, BaseURL: srv.URL, Format: "openai", Auth: "bearer",
					Models: []config.Model{{ID: "m"}},
				}}
				c.Connections = []config.Connection{{ID: "conn", Provider: tc.provider, APIKey: "sk-test"}}
				c.DefaultProvider = tc.provider
				c.Combos = []config.Combo{{
					Name:                 "forced",
					Members:              []string{tc.provider + "/m"},
					ForceReasoningEffort: true,
					ReasoningEffort:      tc.chosen,
				}}
			})
			s := newTestServer(t, cfg, nil)

			res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"forced","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)))
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Code, res.Body)
			}
			if got["reasoning_effort"] != tc.want {
				t.Errorf("upstream reasoning_effort = %v, want %v", got["reasoning_effort"], tc.want)
			}
		})
	}
}

func TestComboForcesReasoningEffortOverClient(t *testing.T) {
	var got map[string]any
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	cfg.Combos = []config.Combo{{
		Name:                 "forced",
		Members:              []string{"testprov/test-model"},
		ForceReasoningEffort: true,
		ReasoningEffort:      "high",
	}}
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"forced","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("upstream reasoning_effort = %v, want high", got["reasoning_effort"])
	}
}

func TestComboForcesReasoningEffortAcrossDialects(t *testing.T) {
	var got map[string]any
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	cfg.Combos = []config.Combo{{
		Name:                 "forced",
		Members:              []string{"testprov/test-model"},
		ForceReasoningEffort: true,
		ReasoningEffort:      "high",
	}}
	s := newTestServer(t, cfg, nil)

	// Anthropic ingress translated to an OpenAI provider still gets the combo
	// override, because it is applied to the final rendered body.
	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"forced","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("upstream reasoning_effort = %v, want high", got["reasoning_effort"])
	}
}

func TestComboWithoutForceKeepsClientEffort(t *testing.T) {
	var got map[string]any
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	cfg.Combos = []config.Combo{{
		Name:    "plain",
		Members: []string{"testprov/test-model"},
	}}
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"plain","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if got["reasoning_effort"] != "low" {
		t.Errorf("upstream reasoning_effort = %v, want low", got["reasoning_effort"])
	}
}
