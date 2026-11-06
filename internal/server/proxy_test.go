package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lgllm/internal/config"
)

// mockUpstream starts a provider stand-in and rewrites testprov's base_url to
// point at it. The returned config is safe to hand to newTestServer.
func mockUpstream(t *testing.T, h http.HandlerFunc) (*httptest.Server, *config.Config) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].BaseURL = srv.URL
	})
	return srv, cfg
}

func TestChatCompletionsPassthrough(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth, gotContentType string
		gotBody                                     map[string]any
	)
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","choices":[{"message":{"content":"hi"}}]}`)
	})
	s := newTestServer(t, cfg, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"user","content":"hello"}],"temperature":0.2}`))
	req.Header.Set("Authorization", "Bearer sk-client")
	res := do(s, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if gotMethod != http.MethodPost || gotPath != "/chat/completions" {
		t.Errorf("upstream request = %s %s", gotMethod, gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("upstream Content-Type = %q", gotContentType)
	}
	if gotAuth != "Bearer sk-upstream" {
		t.Errorf("upstream Authorization = %q, want the provider key", gotAuth)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("upstream model = %v, want the prefix stripped", gotBody["model"])
	}
	if _, ok := gotBody["temperature"]; !ok {
		t.Error("unknown request fields must be forwarded untouched")
	}
	if res.Header().Get("X-Lgllm-Provider") != "testprov" {
		t.Errorf("X-Lgllm-Provider = %q", res.Header().Get("X-Lgllm-Provider"))
	}
	if res.Header().Get("X-Lgllm-Model") != "test-model" {
		t.Errorf("X-Lgllm-Model = %q", res.Header().Get("X-Lgllm-Model"))
	}
	if !strings.Contains(res.Body.String(), `"cmpl-1"`) {
		t.Errorf("body = %s", res.Body)
	}
}

func TestChatCompletionsBareModelKeepsBytes(t *testing.T) {
	var raw string
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	s := newTestServer(t, cfg, nil)

	body := `{"model":"test-model",  "messages": []}`
	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	// The model already resolves to itself, so the original bytes go out. That
	// keeps provider-specific fields byte-exact (float formatting, key order).
	if raw != body {
		t.Errorf("upstream body = %q, want the original %q", raw, body)
	}
}

func TestChatCompletionsStreams(t *testing.T) {
	events := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, e := range events {
			_, _ = io.WriteString(w, e)
			fl.Flush()
		}
	})
	s := newTestServer(t, cfg, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering = %q", rec.Header().Get("X-Accel-Buffering"))
	}
	if got, want := rec.Body.String(), strings.Join(events, ""); got != want {
		t.Errorf("stream = %q, want %q", got, want)
	}
}

func TestChatCompletionsForwardsUpstreamError(t *testing.T) {
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[]}`)))

	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream status", res.Code)
	}
	if res.Body.String() != `{"error":{"message":"rate limited"}}` {
		t.Errorf("body = %s", res.Body)
	}
	if res.Header().Get("Retry-After") != "7" {
		t.Errorf("Retry-After = %q", res.Header().Get("Retry-After"))
	}
	if res.Header().Get("X-Lgllm-Provider") != "testprov" {
		t.Errorf("X-Lgllm-Provider = %q", res.Header().Get("X-Lgllm-Provider"))
	}
}

func TestChatCompletionsRejectsUnsupportedFormat(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].Format = "ollama" // native /api/chat has no translator
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[]}`)))
	if res.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 for a format with no translator", res.Code)
	}
	if !strings.Contains(res.Body.String(), "translator") {
		t.Errorf("body = %s, want an explanation naming the missing translator", res.Body)
	}
}

func TestChatCompletionsRejectsBadInput(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)

	cases := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"not json", `{`},
		{"missing model", `{"messages":[]}`},
		{"model is not a string", `{"model":42}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body)))
			if res.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", res.Code, res.Body)
			}
		})
	}
}

func TestChatCompletionsAmbiguousBareModel(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.DefaultProvider = ""
		c.Providers = append(c.Providers, config.Provider{
			ID: "second", BaseURL: "https://second.invalid", Format: "openai", Auth: "bearer",
			Models: []config.Model{{ID: "m"}},
		})
		c.Connections = append(c.Connections, config.Connection{
			ID: "second-conn", Provider: "second", APIKey: "sk-2",
		})
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"who-knows","messages":[]}`)))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when no default can be inferred", res.Code)
	}
	if !strings.Contains(res.Body.String(), "provider/model") {
		t.Errorf("error should show the prefix form: %s", res.Body)
	}
}

func TestChatCompletionsClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamSawCancel := make(chan struct{})
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamSawCancel)
	})
	s := newTestServer(t, cfg, nil)
	front := httptest.NewServer(s.Handler())
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close() // simulates the client going away

	select {
	case <-upstreamSawCancel:
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream request was not cancelled when the client disconnected")
	}
}

func TestModelsListsAliasPrefixedIDs(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload.Object != "list" {
		t.Errorf("object = %q", payload.Object)
	}
	if len(payload.Data) != 1 {
		t.Fatalf("data = %+v", payload.Data)
	}
	if payload.Data[0].ID != "testprov/test-model" {
		t.Errorf("id = %q", payload.Data[0].ID)
	}
	if payload.Data[0].OwnedBy != "testprov" {
		t.Errorf("owned_by = %q", payload.Data[0].OwnedBy)
	}
}

func TestModelsIsEmptyWithoutProviders(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers = nil
		c.Connections = nil
		c.DefaultProvider = ""
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), `"data":[]`) {
		t.Errorf("body = %s, want an empty list", res.Body)
	}
}

func TestUnknownV1RouteIsJSON(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)
	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d", res.Code)
	}
	if ct := res.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// TestChatCompletionsWithoutProvidersIs503 separates "nothing is configured"
// from "the model name was wrong": the first is an operator problem and is
// reported as such instead of a 404 that reads like a bad model id.
func TestChatCompletionsWithoutProvidersIs503(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers = nil
		c.Connections = nil
		c.DefaultProvider = ""
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"anything","messages":[]}`)))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "connection") {
		t.Errorf("body = %s, want it to name the missing connection", res.Body)
	}
}

// TestStreamSendsHeadersBeforeFirstToken guards the flush after WriteHeader:
// without it net/http holds the status line until the first 32 KiB of body or
// the end of the handler, so a client cannot tell the stream has opened.
func TestStreamSendsHeadersBeforeFirstToken(t *testing.T) {
	release := make(chan struct{})
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Open the stream but withhold the first token until the front end has
		// had a chance to forward the headers.
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	defer close(release)
	s := newTestServer(t, cfg, nil)

	front := httptest.NewServer(s.Handler())
	defer front.Close()

	res, err := front.Client().Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"testprov/test-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("the proxy did not send headers before the first token: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestUpstreamTimeoutIs504(t *testing.T) {
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// Never answer within the test's patience. httptest.Server.Close waits
		// for running handlers, so the hold is short: only long enough for the
		// provider's 100 ms header timeout to fire.
		select {
		case <-r.Context().Done():
		case <-time.After(400 * time.Millisecond):
		}
	})
	cfg.Providers[0].Timeout = config.Duration(100 * time.Millisecond)
	s := newTestServer(t, cfg, nil)

	start := time.Now()
	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[]}`)))
	if res.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body %s)", res.Code, res.Body)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s, the provider timeout was not applied", elapsed)
	}
}

func TestUpstreamConnectionFailureIs502(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		// A port that nothing listens on: dial fails fast.
		c.Providers[0].BaseURL = "http://127.0.0.1:1"
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[]}`)))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", res.Code, res.Body)
	}
}

func TestReadyzReportsProviderCount(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), `"providers":1`) {
		t.Errorf("body = %s", res.Body)
	}
}

func TestReadyzWithoutProvidersIs503(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers = nil
		c.Connections = nil
		c.DefaultProvider = ""
	})
	s := newTestServer(t, cfg, nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when nothing can serve", res.Code)
	}
}
