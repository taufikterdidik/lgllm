package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lgllm/internal/config"
)

// anthropicUpstream points the standard test provider at a mock that speaks the
// Anthropic Messages API.
func anthropicUpstream(t *testing.T, h http.HandlerFunc) (*httptest.Server, *config.Config) {
	t.Helper()
	srv, cfg := mockUpstream(t, h)
	cfg.Providers[0].Format = "anthropic"
	cfg.Providers[0].Auth = "x-api-key"
	return srv, cfg
}

func TestChatCompletionsTranslatesToAnthropic(t *testing.T) {
	var (
		gotPath, gotKey string
		gotBody         map[string]any
	)
	srv, cfg := anthropicUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "id":"msg_1","type":"message","role":"assistant","model":"test-model",
		  "content":[{"type":"text","text":"hello back"}],
		  "stop_reason":"end_turn",
		  "usage":{"input_tokens":4,"output_tokens":2}
		}`)
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if gotPath != "/messages" {
		t.Errorf("upstream path = %q, want /messages", gotPath)
	}
	if gotKey != "sk-upstream" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotBody["system"] != "be terse" {
		t.Errorf("upstream system = %v", gotBody["system"])
	}
	if gotBody["max_tokens"] == nil {
		t.Error("max_tokens was not defaulted")
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("body = %s: %v", res.Body, err)
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 {
		t.Fatalf("response = %s", res.Body)
	}
	if out.Choices[0].Message.Content != "hello back" || out.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v", out.Choices[0])
	}
	if out.Usage.PromptTokens != 4 || out.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", out.Usage)
	}
	if res.Header().Get("X-Lgllm-Model") != "test-model" {
		t.Errorf("X-Lgllm-Model = %q", res.Header().Get("X-Lgllm-Model"))
	}
	_ = srv
}

func TestChatCompletionsTranslatesAnthropicStream(t *testing.T) {
	_, cfg := anthropicUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, ev := range []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":3}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = io.WriteString(w, ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if got := res.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q", got)
	}
	body := res.Body.String()
	for _, want := range []string{`"content":"Hi"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "event: message_start") {
		t.Error("Anthropic event framing leaked into an OpenAI stream")
	}
}

func TestMessagesIngressToOpenAIProvider(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "id":"chatcmpl-1","object":"chat.completion","model":"test-model",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"hi from openai"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
		}`)
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{
		  "model":"testprov/test-model","max_tokens":32,
		  "system":"be terse",
		  "messages":[{"role":"user","content":"hi"}]
		}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotBody["max_tokens"] != float64(32) {
		t.Errorf("upstream max_tokens = %v", gotBody["max_tokens"])
	}
	msgs := gotBody["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("system message = %v", msgs[0])
	}

	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("body = %s: %v", res.Body, err)
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("envelope = %+v", out)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "hi from openai" {
		t.Errorf("content = %+v", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", out.StopReason)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestMessagesIngressAnthropicPassthrough(t *testing.T) {
	var gotBody map[string]any
	_, cfg := anthropicUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "id":"msg_1","type":"message","role":"assistant","model":"test-model",
		  "content":[{"type":"text","text":"direct"}],
		  "stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}
		}`)
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"testprov/test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if gotBody["max_tokens"] != float64(16) {
		t.Errorf("upstream body = %v", gotBody)
	}
	if !strings.Contains(res.Body.String(), `"text":"direct"`) {
		t.Errorf("body = %s, want the provider message relayed", res.Body)
	}
	if res.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", res.Header().Get("Content-Type"))
	}
}

func TestMessagesIngressStreamsFromOpenAI(t *testing.T) {
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, ev := range []string{
			"data: {\"id\":\"c1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n",
			"data: {\"id\":\"c1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n",
			"data: {\"id\":\"c1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n",
			"data: {\"id\":\"c1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"testprov/test-model","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	body := res.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"text":"Hel"`,
		`"text":"lo"`,
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Error("OpenAI [DONE] sentinel leaked into an Anthropic stream")
	}
}

func TestMessagesIngressErrorUsesAnthropicEnvelope(t *testing.T) {
	_, cfg := anthropicUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"testprov/test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", res.Code)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("body = %s: %v", res.Body, err)
	}
	if out.Type != "error" || out.Error.Message != "slow down" {
		t.Errorf("body = %s", res.Body)
	}
}
