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
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n",
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

	// The trailing usage chunk is read by the tap, so the ring entry has real
	// token counts instead of the zero it used to record before the relay.
	usageRes := do(s, httptest.NewRequest(http.MethodGet, "/usage?limit=1", nil))
	if usageRes.Code != http.StatusOK {
		t.Fatalf("usage status = %d, body = %s", usageRes.Code, usageRes.Body)
	}
	var usage struct {
		Entries []struct {
			Stream           bool `json:"stream"`
			PromptTokens     int  `json:"prompt_tokens"`
			CompletionTokens int  `json:"completion_tokens"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(usageRes.Body.Bytes(), &usage); err != nil {
		t.Fatalf("usage body = %s: %v", usageRes.Body, err)
	}
	if len(usage.Entries) != 1 {
		t.Fatalf("usage entries = %+v", usage.Entries)
	}
	entry := usage.Entries[0]
	if !entry.Stream || entry.PromptTokens != 11 || entry.CompletionTokens != 7 {
		t.Errorf("stream usage = %+v", entry)
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

func TestModelDetailReturnsRoutableModel(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].Alias = "tp"
		c.Providers[0].Models = []config.Model{{ID: "vendor/model"}}
		c.Aliases = map[string]string{"fast": "testprov/vendor/model"}
	})
	s := newTestServer(t, cfg, nil)

	for _, path := range []string{
		"/v1/models/tp/vendor/model",
		"/api/v1/models/tp/vendor/model",
		"/v1/models/fast",
	} {
		res := do(s, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", path, res.Code, res.Body)
		}
		var got modelObject
		if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s body is not JSON: %v", path, err)
		}
		if got.Object != "model" || got.ID == "" {
			t.Errorf("%s = %+v", path, got)
		}
		if got.OwnedBy != "testprov" {
			t.Errorf("%s owned_by = %q", path, got.OwnedBy)
		}
	}
}

func TestModelDetailUnknownProviderIs404(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.DefaultProvider = ""
		c.Providers = append(c.Providers, config.Provider{
			ID: "other", BaseURL: "https://other.invalid/v1", Format: "openai", Auth: "bearer",
			Models: []config.Model{{ID: "test-model"}},
		})
		c.Connections = append(c.Connections, config.Connection{
			ID: "other-conn", Provider: "other", APIKey: "sk-other",
		})
	})
	s := newTestServer(t, cfg, nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/models/nope/model", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if ct := res.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(res.Body.String(), "not found") {
		t.Errorf("body = %s", res.Body)
	}
}

func TestComboContextUsesSmallestMember(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].Models[0].Context = 128000
		c.Providers = append(c.Providers, config.Provider{
			ID: "large", BaseURL: "https://large.invalid/v1", Format: "openai", Auth: "bearer",
			Models: []config.Model{{ID: "big-model", Context: 1000000}},
		})
		c.Connections = append(c.Connections, config.Connection{
			ID: "large-conn", Provider: "large", APIKey: "sk-large",
		})
		c.Combos = []config.Combo{{
			Name: "mix", Strategy: "priority",
			Members: []string{"testprov/test-model", "large/big-model"},
		}}
		c.Aliases = map[string]string{"small-alias": "testprov/test-model"}
	})
	s := newTestServer(t, cfg, nil)

	detail := do(s, httptest.NewRequest(http.MethodGet, "/v1/model/mix", nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", detail.Code, detail.Body)
	}
	var body struct {
		Data struct {
			ID            string               `json:"id"`
			ContextLength int                  `json:"context_length"`
			ContextKnown  bool                 `json:"context_known"`
			Members       []modelContextMember `json:"members"`
		} `json:"data"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.ID != "mix" || body.Data.ContextLength != 128000 || !body.Data.ContextKnown {
		t.Errorf("detail = %+v", body.Data)
	}
	if len(body.Data.Members) != 2 {
		t.Errorf("members = %+v", body.Data.Members)
	}

	flat := do(s, httptest.NewRequest(http.MethodGet, "/v1/models/mix", nil))
	var model modelObject
	if err := json.Unmarshal(flat.Body.Bytes(), &model); err != nil {
		t.Fatal(err)
	}
	if model.ContextLength != 128000 || !model.ContextKnown || model.OwnedBy != "combo" {
		t.Errorf("flat = %+v", model)
	}

	list := do(s, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(list.Body.String(), `"id":"mix"`) ||
		!strings.Contains(list.Body.String(), `"context_length":128000`) {
		t.Errorf("list missing combo context: %s", list.Body)
	}

	show := do(s, httptest.NewRequest(http.MethodGet, "/api/show?model=mix", nil))
	if !strings.Contains(show.Body.String(), `"lgllm.context_length":128000`) {
		t.Errorf("show missing combo context: %s", show.Body)
	}
	props := do(s, httptest.NewRequest(http.MethodGet, "/v1/props?model=mix", nil))
	if !strings.Contains(props.Body.String(), `"n_ctx":128000`) {
		t.Errorf("props missing combo context: %s", props.Body)
	}

	alias := do(s, httptest.NewRequest(http.MethodGet, "/v1/model/small-alias", nil))
	if alias.Code != http.StatusOK || !strings.Contains(alias.Body.String(), `"context_length":128000`) {
		t.Errorf("alias detail = %d %s", alias.Code, alias.Body)
	}
}

func TestComboContextUnknownWhenMemberUnknown(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].Models[0].Context = 128000
		c.Providers = append(c.Providers, config.Provider{
			ID: "unknown", BaseURL: "https://unknown.invalid/v1", Format: "openai", Auth: "bearer",
			Models: []config.Model{{ID: "unknown-model"}},
		})
		c.Connections = append(c.Connections, config.Connection{
			ID: "unknown-conn", Provider: "unknown", APIKey: "sk-unknown",
		})
		c.Combos = []config.Combo{{
			Name: "partial", Members: []string{"testprov/test-model", "unknown/unknown-model"},
		}}
	})
	s := newTestServer(t, cfg, nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/model/partial", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body.Data["context_length"]; ok {
		t.Errorf("unknown member must not advertise a numeric context: %s", res.Body)
	}
	if body.Data["context_known"] != false {
		t.Errorf("missing context_known=false: %s", res.Body)
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

// TestSessionHeaderIsStablePerConversation exercises the whole gateway path:
// two turns that share a first user message must carry the same outbound
// session id, while an unrelated conversation must carry a different one.
func TestSessionHeaderIsStablePerConversation(t *testing.T) {
	var sessions []string
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		sessions = append(sessions, r.Header.Get("X-Opencode-Session"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	cfg.Providers[0].SessionHeader = "x-opencode-session"
	s := newTestServer(t, cfg, nil)

	turn := func(first, latest string) {
		t.Helper()
		body := `{"model":"testprov/test-model","messages":[` +
			`{"role":"user","content":"` + first + `"},` +
			`{"role":"assistant","content":"ok"},` +
			`{"role":"user","content":"` + latest + `"}]}`
		res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(body)))
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", res.Code, res.Body)
		}
	}

	turn("fix the failing test", "it still fails")
	turn("fix the failing test", "thanks, now run it again")
	turn("write a different feature", "start with the schema")

	if len(sessions) != 3 {
		t.Fatalf("upstream saw %d requests, want 3", len(sessions))
	}
	for i, got := range sessions {
		if got == "" {
			t.Fatalf("request %d sent no X-Opencode-Session", i)
		}
	}
	if sessions[0] != sessions[1] {
		t.Errorf("same conversation moved from %q to %q", sessions[0], sessions[1])
	}
	if sessions[0] == sessions[2] {
		t.Errorf("different conversations share %q", sessions[0])
	}
}

// TestSessionHeaderPrefersClientValue proves a caller can pin the id, which is
// what agents that already track a session expect.
func TestSessionHeaderPrefersClientValue(t *testing.T) {
	var got string
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Opencode-Session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	cfg.Providers[0].SessionHeader = "x-opencode-session"
	s := newTestServer(t, cfg, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Session-Id", "agent-session-42")
	res := do(s, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if got != "agent-session-42" {
		t.Errorf("upstream session = %q, want the client value", got)
	}
}

// TestSessionHeaderAbsentWithoutCapability keeps the feature opt-in: a provider
// that declares no session header must see no extra request header.
func TestSessionHeaderAbsentWithoutCapability(t *testing.T) {
	var got string
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Opencode-Session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if got != "" {
		t.Errorf("provider without session_header received %q", got)
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
