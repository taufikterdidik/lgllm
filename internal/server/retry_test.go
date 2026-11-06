package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"lgllm/internal/config"
)

// providerAt declares one OpenAI-format provider at url and returns its id.
func providerAt(c *config.Config, id, url, key string) {
	c.Providers = append(c.Providers, config.Provider{
		ID: id, BaseURL: url, Format: "openai", Auth: "bearer",
		Models: []config.Model{{ID: id + "-model"}},
	})
	c.Connections = append(c.Connections, config.Connection{
		ID: id + "-conn", Provider: id, APIKey: key,
	})
}

func jsonUpstream(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sseUpstream(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, ev := range events {
			_, _ = io.WriteString(w, ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestComboFallsBackOnProviderError(t *testing.T) {
	bad := jsonUpstream(t, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	good := jsonUpstream(t, `{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"from good"},"finish_reason":"stop"}]}`, http.StatusOK)

	cfg := testConfig(t, func(c *config.Config) {
		providerAt(c, "bad", bad.URL, "sk-bad")
		providerAt(c, "good", good.URL, "sk-good")
		c.Combos = []config.Combo{{
			Name:    "combo",
			Members: []string{"bad/bad-model", "good/good-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "from good") {
		t.Errorf("body = %s, want the second member's answer", res.Body)
	}
	if got := res.Header().Get("X-Lgllm-Provider"); got != "good" {
		t.Errorf("X-Lgllm-Provider = %q, want the member that answered", got)
	}
}

func TestComboFallsBackOnBadRequestWithoutCooling(t *testing.T) {
	// A 400 is request-scoped: the account is healthy, so the next member is
	// tried immediately and no cooldown is recorded.
	bad := jsonUpstream(t, `{"error":{"message":"bad parameter"}}`, http.StatusBadRequest)
	good := jsonUpstream(t, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, http.StatusOK)

	cfg := testConfig(t, func(c *config.Config) {
		providerAt(c, "bad", bad.URL, "sk-bad")
		providerAt(c, "good", good.URL, "sk-good")
		c.Combos = []config.Combo{{
			Name:    "combo",
			Members: []string{"bad/bad-model", "good/good-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if s.Balancer.Cooling("bad-conn") {
		t.Error("a 400 must not cool the account down")
	}
}

func TestComboAllFailReturns502Summary(t *testing.T) {
	first := jsonUpstream(t, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	second := jsonUpstream(t, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)

	cfg := testConfig(t, func(c *config.Config) {
		providerAt(c, "one", first.URL, "sk-one")
		providerAt(c, "two", second.URL, "sk-two")
		c.Combos = []config.Combo{{
			Name:    "combo",
			Members: []string{"one/one-model", "two/two-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Attempts []attemptFailure `json:"attempts"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("body = %s: %v", res.Body, err)
	}
	if len(body.Attempts) != 2 {
		t.Fatalf("attempts = %+v", body.Attempts)
	}
	if !strings.Contains(body.Error.Message, "rate limited") || !strings.Contains(body.Error.Message, "overloaded") {
		t.Errorf("message = %q, want both provider errors", body.Error.Message)
	}
	if !s.Balancer.Cooling("one-conn") || !s.Balancer.Cooling("two-conn") {
		t.Error("both failing credentials should be cooling down")
	}
}

func TestMultiAccountRotatesAfterFailure(t *testing.T) {
	var hits atomic.Int64
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(good.Close)
	bad := jsonUpstream(t, `{"error":{"message":"invalid key"}}`, http.StatusUnauthorized)

	cfg := testConfig(t, func(c *config.Config) {
		// One provider, two credentials: the first is invalid, the second works.
		c.Connections = []config.Connection{
			{ID: "bad-conn", Provider: "testprov", APIKey: "sk-bad", BaseURL: bad.URL},
			{ID: "good-conn", Provider: "testprov", APIKey: "sk-good", BaseURL: good.URL},
		}
		c.DefaultProvider = "testprov"
	})
	s := newTestServer(t, cfg, nil)

	body := `{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if res.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, res.Code, res.Body)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("good credential served %d requests, want 2", got)
	}
	if !s.Balancer.Cooling("bad-conn") {
		t.Error("the invalid credential should be cooling down")
	}
}

func TestStickySessionKeepsOneConnection(t *testing.T) {
	newServer := func(tag string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"`+tag+`"},"finish_reason":"stop"}]}`)
		}))
	}
	a, b := newServer("a"), newServer("b")
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)

	cfg := testConfig(t, func(c *config.Config) {
		c.Connections = []config.Connection{
			{ID: "a", Provider: "testprov", APIKey: "sk-a", BaseURL: a.URL},
			{ID: "b", Provider: "testprov", APIKey: "sk-b", BaseURL: b.URL},
		}
	})
	s := newTestServer(t, cfg, nil)

	var first string
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("X-Lgllm-Session", "conversation-42")
		res := do(s, req)
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d", res.Code)
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(res.Body.Bytes(), &out)
		got := out.Choices[0].Message.Content
		if first == "" {
			first = got
		} else if got != first {
			t.Fatalf("session moved from %q to %q", first, got)
		}
	}
}

func TestAnthropicIngressFallsBackWithAnthropicError(t *testing.T) {
	bad := jsonUpstream(t, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, http.StatusServiceUnavailable)
	good := jsonUpstream(t, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, http.StatusOK)

	cfg := testConfig(t, func(c *config.Config) {
		providerAt(c, "bad", bad.URL, "sk-bad")
		providerAt(c, "good", good.URL, "sk-good")
		c.Combos = []config.Combo{{
			Name:    "combo",
			Members: []string{"bad/bad-model", "good/good-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"combo","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), `"text":"ok"`) {
		t.Errorf("body = %s, want the fallback translated to Anthropic", res.Body)
	}
}

func TestCoolingComboMemberIsSkippedNextTime(t *testing.T) {
	var badHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	t.Cleanup(bad.Close)
	good := jsonUpstream(t, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, http.StatusOK)

	cfg := testConfig(t, func(c *config.Config) {
		providerAt(c, "bad", bad.URL, "sk-bad")
		providerAt(c, "good", good.URL, "sk-good")
		c.Combos = []config.Combo{{
			Name:    "combo",
			Members: []string{"bad/bad-model", "good/good-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)

	body := `{"model":"combo","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 3; i++ {
		res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if res.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, res.Code, res.Body)
		}
	}
	if got := badHits.Load(); got != 1 {
		t.Errorf("the failing member was called %d times, want only the first request", got)
	}
}

func TestStreamFallsBackWhenFirstMemberSendsErrorEvent(t *testing.T) {
	broken := sseUpstream(t,
		"data: {\"error\":{\"message\":\"upstream failed after 200\"}}\n\n")
	good := sseUpstream(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"recovered\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n")

	cfg := testConfig(t, func(c *config.Config) {
		providerAt(c, "broken", broken.URL, "sk-broken")
		providerAt(c, "good", good.URL, "sk-good")
		c.Combos = []config.Combo{{
			Name:    "combo",
			Members: []string{"broken/broken-model", "good/good-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"combo","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	body := res.Body.String()
	if !strings.Contains(body, "recovered") {
		t.Errorf("body = %s, want the second member's stream", body)
	}
	if strings.Contains(body, "upstream failed") {
		t.Errorf("the first member's error leaked to the client: %s", body)
	}
}
