package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"lgllm/internal/config"
	"lgllm/internal/registry"
)

func systemOneUpstream(t *testing.T, fn func(*http.Request) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path = %q, want /v1/systemone", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %q", got)
		}
		status, body := fn(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func systemOneConfig(t *testing.T, providers ...config.Provider) *config.Config {
	t.Helper()
	return testConfig(t, func(c *config.Config) {
		c.Providers = providers
		c.Connections = make([]config.Connection, 0, len(providers))
		for _, p := range providers {
			c.Connections = append(c.Connections, config.Connection{
				ID: p.ID + "-conn", Provider: p.ID, APIKey: "sk-test",
			})
		}
		if len(providers) > 0 {
			c.DefaultProvider = providers[0].ID
		}
	})
}

func systemOneProvider(id, baseURL string) config.Provider {
	return config.Provider{
		ID: id, BaseURL: baseURL, Format: registry.FormatSystemOne, Auth: "bearer",
		Models: []config.Model{{ID: "typesafe/jev"}},
	}
}

func TestSystemOneProxiesAndRewritesModel(t *testing.T) {
	var gotBody atomic.Value
	up := systemOneUpstream(t, func(r *http.Request) (int, string) {
		body, _ := io.ReadAll(r.Body)
		gotBody.Store(string(body))
		return http.StatusOK, `{"model":"typesafe/jev","answers":{"urgent":{"type":"noul","noul":0.93}},"usage":{"input_tokens":286,"output_tokens":20}}`
	})
	cfg := systemOneConfig(t, systemOneProvider("cmds", up.URL+"/v1"))
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"cmds/typesafe/jev","state":"payments failed","questions":{"urgent":{"type":"noul","instructions":"urgent?"}}}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), `"noul":0.93`) {
		t.Errorf("body = %s, want the upstream decision", res.Body)
	}
	if got := res.Header().Get("X-Lgllm-Provider"); got != "commandcode-systemone" {
		t.Errorf("X-Lgllm-Provider = %q", got)
	}
	if got := res.Header().Get("X-Lgllm-Model"); got != "typesafe/jev" {
		t.Errorf("X-Lgllm-Model = %q", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody.Load().(string)), &sent); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if sent["model"] != "typesafe/jev" {
		t.Errorf("forwarded model = %v, want the upstream id", sent["model"])
	}
	if sent["state"] != "payments failed" {
		t.Errorf("forwarded state = %v", sent["state"])
	}
}

func TestSystemOneComboFallback(t *testing.T) {
	bad := systemOneUpstream(t, func(r *http.Request) (int, string) {
		return http.StatusInternalServerError, `{"error":{"message":"boom"}}`
	})
	good := systemOneUpstream(t, func(r *http.Request) (int, string) {
		return http.StatusOK, `{"answers":{"ok":{"type":"noul","noul":0.9}}}`
	})
	p1 := systemOneProvider("bad", bad.URL+"/v1")
	p2 := systemOneProvider("good", good.URL+"/v1")
	cfg := systemOneConfig(t, p1, p2)
	cfg.Combos = []config.Combo{{Name: "decide", Members: []string{"bad/typesafe/jev", "good/typesafe/jev"}}}
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"decide","state":"x","questions":{}}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), `"noul":0.9`) {
		t.Errorf("body = %s, want the second member", res.Body)
	}
}

func TestSystemOneRejectsChatModelAndStreaming(t *testing.T) {
	up := systemOneUpstream(t, func(r *http.Request) (int, string) {
		return http.StatusOK, `{}`
	})
	chat := config.Provider{
		ID: "chat", BaseURL: up.URL + "/v1", Format: registry.FormatChat, Auth: "bearer",
		Models: []config.Model{{ID: "model"}},
	}
	cfg := systemOneConfig(t, systemOneProvider("cmds", up.URL+"/v1"), chat)
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"chat/model","state":"x","questions":{}}`)))
	if res.Code != http.StatusBadRequest {
		t.Errorf("chat model status = %d, body = %s", res.Code, res.Body)
	}

	res = do(s, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"cmds/typesafe/jev","stream":true}`)))
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "streaming") {
		t.Errorf("stream status = %d, body = %s", res.Code, res.Body)
	}
}

func TestModelsListHidesSystemOne(t *testing.T) {
	up := systemOneUpstream(t, func(r *http.Request) (int, string) { return http.StatusOK, `{}` })
	cfg := systemOneConfig(t, systemOneProvider("cmds", up.URL+"/v1"))
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if bytes.Contains(res.Body.Bytes(), []byte("typesafe/jev")) {
		t.Errorf("decision model leaked into /v1/models: %s", res.Body)
	}
}

func TestPerformanceHidesSystemOne(t *testing.T) {
	up := systemOneUpstream(t, func(r *http.Request) (int, string) { return http.StatusOK, `{}` })
	s, _, _ := newAdminTestServerWith(t, false, func(c *config.Config) {
		c.Providers = []config.Provider{systemOneProvider("cmds", up.URL+"/v1")}
		c.Connections = []config.Connection{{ID: "cmds-conn", Provider: "cmds", APIKey: "sk-test"}}
		c.DefaultProvider = "cmds"
	}, nil)

	res := do(s, adminRequest(http.MethodGet, "/api/performance", adminTestToken, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if bytes.Contains(res.Body.Bytes(), []byte("typesafe/jev")) {
		t.Errorf("decision model leaked into performance: %s", res.Body)
	}
}

func TestChatRejectsSystemOneModel(t *testing.T) {
	up := systemOneUpstream(t, func(r *http.Request) (int, string) { return http.StatusOK, `{}` })
	cfg := systemOneConfig(t, systemOneProvider("cmds", up.URL+"/v1"))
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cmds/typesafe/jev","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "/v1/systemone") {
		t.Errorf("body = %s, want a pointer to /v1/systemone", res.Body)
	}
}
