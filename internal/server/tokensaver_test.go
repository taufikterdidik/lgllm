package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lgllm/internal/config"
	"lgllm/internal/tokensaver"
)

// bigToolResult is valid JSON well over the 2 KB default threshold, with the
// verbose fields the saver drops.
func bigToolResult(t *testing.T) string {
	t.Helper()
	type item struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		NodeID    string `json:"node_id"`
		AvatarURL string `json:"avatar_url"`
		CreatedAt string `json:"created_at"`
	}
	items := make([]item, 0, 60)
	for i := 0; i < 60; i++ {
		items = append(items, item{
			ID: i, Name: strings.Repeat("value", 12),
			NodeID: strings.Repeat("n", 50), AvatarURL: strings.Repeat("u", 80),
			CreatedAt: "2026-01-01T00:00:00Z",
		})
	}
	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func toolRequestBody(t *testing.T, content string, extra string) string {
	t.Helper()
	payload := map[string]any{
		"model": "testprov/test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "inspect this"},
			map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "read_file", "arguments": `{"path":"x"}`},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": content},
		},
	}
	if extra != "" {
		var extraMap map[string]any
		if err := json.Unmarshal([]byte(extra), &extraMap); err != nil {
			t.Fatal(err)
		}
		for k, v := range extraMap {
			payload[k] = v
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// upstreamToolContent returns the tool message content the provider received,
// plus the whole body for further assertions.
func upstreamBody(t *testing.T, cfg *config.Config, reqBody string, headers map[string]string) (map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	cfg.Providers[0].BaseURL = srv.URL

	s := newTestServer(t, cfg, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res := do(s, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	return got, res
}

func upstreamToolContent(t *testing.T, cfg *config.Config, reqBody string, headers map[string]string) (string, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	got, res := upstreamBody(t, cfg, reqBody, headers)
	msgs, _ := got["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if msg["role"] == "tool" {
			content, _ := msg["content"].(string)
			return content, got, res
		}
	}
	t.Fatalf("no tool message reached the upstream: %v", got)
	return "", nil, nil
}

func TestTokenSaverCompressesToolResults(t *testing.T) {
	content := bigToolResult(t)
	cfg := testConfig(t, nil)
	got, _, res := upstreamToolContent(t, cfg, toolRequestBody(t, content, ""), nil)

	if len(got) >= len(content) {
		t.Fatalf("tool result was not shrunk: %d -> %d", len(content), len(got))
	}
	if !strings.Contains(got, tokensaver.Marker) {
		t.Error("compressed content is missing the marker")
	}
	if !strings.Contains(got, "value") {
		t.Error("the head of the JSON should survive")
	}
	header := res.Header().Get("X-Lgllm-Token-Saver")
	if !strings.HasPrefix(header, "on; saved=") {
		t.Errorf("X-Lgllm-Token-Saver = %q", header)
	}
}

func TestTokenSaverBypassHeader(t *testing.T) {
	content := bigToolResult(t)
	cfg := testConfig(t, nil)
	got, _, res := upstreamToolContent(t, cfg, toolRequestBody(t, content, ""),
		map[string]string{"X-Lgllm-Token-Saver": "off"})

	if got != content {
		t.Errorf("content changed despite the bypass: %d -> %d bytes", len(content), len(got))
	}
	if header := res.Header().Get("X-Lgllm-Token-Saver"); header != "" {
		t.Errorf("X-Lgllm-Token-Saver = %q, want it absent", header)
	}
}

func TestTokenSaverCanBeDisabledPerCombo(t *testing.T) {
	content := bigToolResult(t)
	disabled := false
	cfg := testConfig(t, func(c *config.Config) {
		c.Combos = []config.Combo{{
			Name:       "coding",
			Members:    []string{"testprov/test-model"},
			TokenSaver: &disabled,
		}}
	})
	body := toolRequestBody(t, content, "")
	body = strings.Replace(body, `"model":"testprov/test-model"`, `"model":"coding"`, 1)
	got, _, _ := upstreamToolContent(t, cfg, body, nil)
	if got != content {
		t.Error("the combo opt-out did not disable the saver")
	}
}

func TestTokenSaverPreservesUnknownRequestFields(t *testing.T) {
	content := bigToolResult(t)
	cfg := testConfig(t, nil)
	// response_format is outside the pivot IR; enabling the saver must not drop
	// it when the provider speaks the same dialect as the client.
	extra := `{"response_format":{"type":"json_object"},"parallel_tool_calls":false}`
	_, whole, _ := upstreamToolContent(t, cfg, toolRequestBody(t, content, extra), nil)

	if _, ok := whole["response_format"]; !ok {
		t.Errorf("response_format was dropped: %v", whole)
	}
	if _, ok := whole["parallel_tool_calls"]; !ok {
		t.Errorf("parallel_tool_calls was dropped: %v", whole)
	}
}

func TestTokenSaverPromptIsInjected(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.TokenSaver.Prompt = "answer like a caveman"
	})
	body := `{"model":"testprov/test-model","messages":[{"role":"system","content":"be helpful"},{"role":"user","content":"hi"}]}`
	whole, _ := upstreamBody(t, cfg, body, nil)

	msgs, _ := whole["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].(string)
	if first["role"] != "system" || !strings.Contains(content, "answer like a caveman") {
		t.Errorf("system prompt = %v", first)
	}
	if !strings.Contains(content, "be helpful") {
		t.Errorf("the original system prompt was lost: %q", content)
	}
}
