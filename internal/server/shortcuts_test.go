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

// shortcutConfig adds an alias, a combo, and a second provider to the standard
// test config. The second provider has no credential, so a combo has a member it
// must skip.
func shortcutConfig(t *testing.T) *config.Config {
	t.Helper()
	return testConfig(t, func(c *config.Config) {
		c.Aliases = map[string]string{"fast": "testprov/test-model"}
		c.Combos = []config.Combo{{
			Name:    "coding",
			Members: []string{"testprov/test-model"},
		}}
	})
}

func TestModelsListsAliasesAndCombos(t *testing.T) {
	s := newTestServer(t, shortcutConfig(t), nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var body struct {
		Data []modelObject `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("body = %s: %v", res.Body, err)
	}

	byID := make(map[string]modelObject, len(body.Data))
	for _, m := range body.Data {
		byID[m.ID] = m
	}
	if m, ok := byID["fast"]; !ok {
		t.Error("the alias is missing from /v1/models")
	} else if m.OwnedBy != "testprov" {
		t.Errorf("alias owned_by = %q, want its provider", m.OwnedBy)
	}
	if m, ok := byID["coding"]; !ok {
		t.Error("the combo is missing from /v1/models")
	} else if m.OwnedBy != "combo" {
		t.Errorf("combo owned_by = %q", m.OwnedBy)
	}
}

func TestAPITagsListsAliasesAndCombos(t *testing.T) {
	s := newTestServer(t, shortcutConfig(t), nil)

	res := do(s, httptest.NewRequest(http.MethodGet, "/api/tags", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var payload struct {
		Models []struct {
			Name         string   `json:"name"`
			Digest       string   `json:"digest"`
			Capabilities []string `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body = %s: %v", res.Body, err)
	}

	byName := make(map[string]bool, len(payload.Models))
	for _, m := range payload.Models {
		if m.Digest == "" {
			t.Errorf("%s has no digest", m.Name)
		}
		byName[m.Name] = true
	}
	if !byName["fast"] || !byName["coding"] {
		t.Errorf("aliases/combos are not listed: %v", byName)
	}
}

func TestAPIShowResolvesAliasAndCombo(t *testing.T) {
	s := newTestServer(t, shortcutConfig(t), nil)

	for _, name := range []string{"fast", "coding"} {
		t.Run(name, func(t *testing.T) {
			res := do(s, httptest.NewRequest(http.MethodGet, "/api/show?model="+name, nil))
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Code, res.Body)
			}
			var payload struct {
				ModelInfo map[string]any `json:"model_info"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body = %s: %v", res.Body, err)
			}
			if got := payload.ModelInfo["lgllm.model"]; got != "test-model" {
				t.Errorf("lgllm.model = %v, want the resolved target", got)
			}
		})
	}
}

func TestChatCompletionsRoutesShortcut(t *testing.T) {
	for _, name := range []string{"fast", "coding"} {
		t.Run(name, func(t *testing.T) {
			var gotModel string
			_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &payload)
				gotModel, _ = payload["model"].(string)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"x","choices":[]}`)
			})
			cfg.Aliases = map[string]string{"fast": "testprov/test-model"}
			cfg.Combos = []config.Combo{{
				Name:    "coding",
				Members: []string{"testprov/test-model"},
			}}
			s := newTestServer(t, cfg, nil)

			res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"`+name+`","messages":[{"role":"user","content":"hi"}]}`)))
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Code, res.Body)
			}
			if gotModel != "test-model" {
				t.Errorf("upstream model = %q, want the shortcut's target", gotModel)
			}
			if res.Header().Get("X-Lgllm-Model") != "test-model" {
				t.Errorf("X-Lgllm-Model = %q", res.Header().Get("X-Lgllm-Model"))
			}
		})
	}
}

func TestModelsInfersVendorPrefixWithoutAlias(t *testing.T) {
	var gotModel string
	srv, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		gotModel, _ = payload["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	// Rewrite the only usable provider into the [OI] catalog entry so "gpt-*"
	// inference has a target, keeping the mock upstream's address.
	cfg.Providers = []config.Provider{{
		ID: "openai", BaseURL: srv.URL, Format: "openai", Auth: "bearer",
		Models: []config.Model{{ID: "gpt-4o"}},
	}}
	cfg.Connections = []config.Connection{{ID: "oa", Provider: "openai", APIKey: "sk-oa"}}
	cfg.DefaultProvider = "openai"
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if gotModel != "gpt-4o-mini" {
		t.Errorf("upstream model = %q", gotModel)
	}
	if res.Header().Get("X-Lgllm-Provider") != "openai" {
		t.Errorf("X-Lgllm-Provider = %q, want the inferred provider", res.Header().Get("X-Lgllm-Provider"))
	}
}
