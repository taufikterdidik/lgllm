package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lgllm/internal/config"
)

// TestAPIv1ModelsMatchesV1Models guards the alias: both paths must serve the
// same payload, because a client that discovers /api/v1/models must see exactly
// what /v1/models advertises.
func TestAPIv1ModelsMatchesV1Models(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)

	canonical := do(s, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	alias := do(s, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))

	if alias.Code != http.StatusOK {
		t.Fatalf("status = %d", alias.Code)
	}
	if alias.Body.String() != canonical.Body.String() {
		t.Errorf("/api/v1/models body = %s, want the /v1/models body %s",
			alias.Body, canonical.Body)
	}
}

func TestAPITagsListsOllamaShapedModels(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].Alias = "tp"
		c.Providers[0].Models = []config.Model{{
			ID: "test-model", Context: 32000,
			Capabilities: map[string]bool{"tools": true, "vision": true},
		}}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/api/tags", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}

	var payload struct {
		Models []struct {
			Name         string   `json:"name"`
			Model        string   `json:"model"`
			ModifiedAt   string   `json:"modified_at"`
			Size         int64    `json:"size"`
			Digest       string   `json:"digest"`
			Capabilities []string `json:"capabilities"`
			Details      struct {
				ParentModel string `json:"parent_model"`
				Format      string `json:"format"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, res.Body)
	}
	if len(payload.Models) != 1 {
		t.Fatalf("models = %+v", payload.Models)
	}

	m := payload.Models[0]
	if m.Name != "tp/test-model" || m.Model != "tp/test-model" {
		t.Errorf("name/model = %q/%q, want the alias-prefixed id", m.Name, m.Model)
	}
	if m.Details.ParentModel != "testprov" {
		t.Errorf("details.parent_model = %q", m.Details.ParentModel)
	}
	if m.ModifiedAt == "" {
		t.Error("modified_at must not be empty; Ollama clients display it")
	}
	if got, want := strings.Join(m.Capabilities, ","), "completion,tools,vision"; got != want {
		t.Errorf("capabilities = %q, want %q", got, want)
	}
}

func TestAPITagsIsEmptyWithoutProviders(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers = nil
		c.Connections = nil
		c.DefaultProvider = ""
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/api/tags", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), `"models":[]`) {
		t.Errorf("body = %s, want an empty list", res.Body)
	}
}

func TestAPIShowDescribesModel(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/api/show",
		strings.NewReader(`{"model":"testprov/test-model"}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}

	var payload struct {
		Details   map[string]any `json:"details"`
		ModelInfo map[string]any `json:"model_info"`
		Caps      []string       `json:"capabilities"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, res.Body)
	}
	if payload.Details["parent_model"] != "testprov" {
		t.Errorf("details.parent_model = %v", payload.Details["parent_model"])
	}
	if payload.ModelInfo["lgllm.model"] != "test-model" {
		t.Errorf("model_info = %v", payload.ModelInfo)
	}
	if len(payload.Caps) == 0 || payload.Caps[0] != "completion" {
		t.Errorf("capabilities = %v", payload.Caps)
	}
}

func TestAPIShowAcceptsBareNameAndQuery(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)

	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"deprecated name field", httptest.NewRequest(http.MethodPost, "/api/show",
			strings.NewReader(`{"name":"test-model"}`))},
		{"query string", httptest.NewRequest(http.MethodGet, "/api/show?model=test-model", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := do(s, tc.req)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Code, res.Body)
			}
		})
	}
}

func TestAPIShowUnknownModelIs404(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/api/show",
		strings.NewReader(`{"model":"nope/missing"}`)))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}

	// Ollama errors are a flat string, not the [OI] nested object.
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if !strings.Contains(body.Error, "not found") {
		t.Errorf("error = %q", body.Error)
	}
}

func TestAPIShowWithoutModelIs400(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)
	res := do(s, httptest.NewRequest(http.MethodPost, "/api/show", strings.NewReader(`{}`)))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

func TestV1PropsDescribesDefaultModel(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].Alias = "tp"
		c.Providers[0].Models = []config.Model{{ID: "test-model", Context: 128000}}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/props", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}

	var payload struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		TotalSlots int    `json:"total_slots"`
		ModelAlias string `json:"model_alias"`
		BuildInfo  string `json:"build_info"`
		IsSleeping bool   `json:"is_sleeping"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, res.Body)
	}
	if payload.ModelAlias != "tp/test-model" {
		t.Errorf("model_alias = %q", payload.ModelAlias)
	}
	if payload.DefaultGenerationSettings.NCtx != 128000 {
		t.Errorf("n_ctx = %d, want the configured context", payload.DefaultGenerationSettings.NCtx)
	}
	if payload.TotalSlots != 1 {
		t.Errorf("total_slots = %d", payload.TotalSlots)
	}
	if payload.IsSleeping {
		t.Error("is_sleeping must be false: there is no local KV cache")
	}
	if !strings.HasPrefix(payload.BuildInfo, "lgllm ") {
		t.Errorf("build_info = %q", payload.BuildInfo)
	}
}

func TestV1PropsHonoursModelQuery(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers[0].Models = []config.Model{
			{ID: "first", Context: 1000},
			{ID: "second", Context: 2000},
		}
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/props?model=testprov/second", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var payload struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		ModelAlias string `json:"model_alias"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ModelAlias != "testprov/second" || payload.DefaultGenerationSettings.NCtx != 2000 {
		t.Errorf("props = %+v, want the requested model", payload)
	}
}

func TestV1PropsUnknownModelIs404(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/props?model=missing/model", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
}

func TestAPIShowWithoutProvidersIs404(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Providers = nil
		c.Connections = nil
		c.DefaultProvider = ""
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/api/show",
		strings.NewReader(`{"model":"anything"}`)))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (an Ollama server with no models says the same)", res.Code)
	}
}

// TestCompatRoutesRequireAuth keeps the discovery endpoints behind the same
// policy as /v1: they leak the provider/model catalogue, so they are not public
// the way /healthz is.
func TestCompatRoutesRequireAuth(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Auth.Mode = "required"
		c.Auth.Keys = []string{"sk-0123456789abcdef"}
	})
	s := newTestServer(t, cfg, nil)

	for _, path := range []string{"/api/v1/models", "/api/tags", "/api/show", "/v1/props"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.9:5555"
		res := do(s, req)
		if res.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401 without a key", path, res.Code)
		}
	}
}

func TestPropsNativePathIsServed(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/props", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want the llama.cpp native path to work too", res.Code)
	}
}

func TestPropsToleratesLatestTag(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil), nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/props?model=test-model:latest", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want the Ollama :latest suffix to be tolerated", res.Code)
	}
}
