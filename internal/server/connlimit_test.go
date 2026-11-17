package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lgllm/internal/config"
)

func TestConnectionConcurrencyLimitReturns429(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"slow"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(up.Close)

	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].BaseURL = up.URL
		c.Connections[0].MaxConcurrent = 1
	})
	s := newTestServer(t, cfg, nil)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the upstream")
	}

	second := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body = %s", second.Code, second.Body)
	}
	if got := second.Header().Get("Retry-After"); got == "" {
		t.Error("429 response has no Retry-After header")
	}
	if !strings.Contains(second.Body.String(), "concurrency limit") {
		t.Errorf("body = %s, want a concurrency limit message", second.Body)
	}

	var metrics bytes.Buffer
	if err := s.Metrics.Render(&metrics); err != nil {
		t.Fatalf("render metrics: %v", err)
	}
	if !strings.Contains(metrics.String(),
		`lgllm_connection_concurrency_limited_total{provider="testprov",connection="test-conn"} 1`) {
		t.Errorf("concurrency counter missing:\n%s", metrics.String())
	}

	unblock()
	select {
	case res := <-first:
		if res.Code != http.StatusOK {
			t.Fatalf("first status = %d, want 200; body = %s", res.Code, res.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish after the upstream was released")
	}
}

func TestConnectionConcurrencyFallsThroughToComboMember(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"from slow"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(slow.Close)
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"from fast"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(fast.Close)

	cfg := testConfig(t, func(c *config.Config) {
		providerAt(c, "slow", slow.URL, "sk-slow")
		providerAt(c, "fast", fast.URL, "sk-fast")
		for i := range c.Connections {
			if c.Connections[i].ID == "slow-conn" {
				c.Connections[i].MaxConcurrent = 1
			}
		}
		c.Combos = []config.Combo{{
			Name:    "combo",
			Members: []string{"slow/slow-model", "fast/fast-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)

	body := `{"model":"combo","messages":[{"role":"user","content":"hi"}]}`
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the slow upstream")
	}

	second := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 from the fallback member; body = %s", second.Code, second.Body)
	}
	if !strings.Contains(second.Body.String(), "from fast") {
		t.Errorf("second body = %s, want the fast member", second.Body)
	}

	unblock()
	select {
	case res := <-first:
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "from slow") {
			t.Fatalf("first = %d %s", res.Code, res.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish after the slow upstream was released")
	}
}
