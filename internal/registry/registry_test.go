package registry

import (
	"strings"
	"testing"

	"lgllm/internal/config"
)

// baseConfig returns a config with one catalog provider behind a connection.
func baseConfig() config.Config {
	cfg := config.Default()
	cfg.Auth.Mode = "off"
	cfg.Connections = []config.Connection{
		{ID: "deepseek-main", Provider: "deepseek", APIKey: "sk-ds"},
	}
	return cfg
}

func TestMaterializeInheritsCatalog(t *testing.T) {
	cfg := baseConfig()
	got := Materialize(cfg.Providers, cfg.Connections)

	if len(got) != 1 {
		t.Fatalf("got %d providers, want 1: %+v", len(got), got)
	}
	p := got[0]
	if p.ID != "deepseek" {
		t.Errorf("id = %q", p.ID)
	}
	if p.BaseURL != "https://api.deepseek.com/v1" {
		t.Errorf("base_url = %q, catalog value not inherited", p.BaseURL)
	}
	if p.Format != FormatChat || p.Auth != "bearer" {
		t.Errorf("format/auth = %q/%q", p.Format, p.Auth)
	}
	if len(p.Models) == 0 {
		t.Error("catalog model hints were dropped")
	}
}

func TestOpenCodeGoCatalog(t *testing.T) {
	b, ok := CatalogByRef("opencode-go")
	if !ok {
		t.Fatal("opencode-go is missing from the catalog")
	}
	if b.Alias != "ocg" {
		t.Errorf("alias = %q", b.Alias)
	}
	if b.BaseURL != "https://opencode.ai/inference/go/openai/v1" {
		t.Errorf("base_url = %q", b.BaseURL)
	}
	if b.Format != FormatChat || b.Auth != "bearer" {
		t.Errorf("format/auth = %q/%q", b.Format, b.Auth)
	}
	if b.SessionHeader != "x-opencode-session" {
		t.Errorf("session_header = %q", b.SessionHeader)
	}
	if b.KeyEnv != "OPENCODE_GO_API_KEY" {
		t.Errorf("key_env = %q", b.KeyEnv)
	}
	if len(b.Models) < 20 {
		t.Errorf("models = %d, want the documented Go list", len(b.Models))
	}

	// A connection alone is enough to materialize the provider.
	cfg := config.Default()
	cfg.Connections = []config.Connection{{ID: "go", Provider: "ocg", APIKey: "oc_sk_test"}}
	got := Materialize(cfg.Providers, cfg.Connections)
	if len(got) != 1 || got[0].ID != "opencode-go" {
		t.Fatalf("materialized = %+v", got)
	}
	if got[0].SessionHeader != "x-opencode-session" {
		t.Errorf("materialized session_header = %q", got[0].SessionHeader)
	}
}

func TestCommandCodeCatalog(t *testing.T) {
	chat, ok := CatalogByRef("cmd")
	if !ok {
		t.Fatal("commandcode is missing from the catalog")
	}
	if chat.ID != "commandcode" || chat.Alias != "cmd" {
		t.Errorf("id/alias = %q/%q", chat.ID, chat.Alias)
	}
	if chat.BaseURL != "https://api.commandcode.ai/provider/v1" {
		t.Errorf("base_url = %q", chat.BaseURL)
	}
	if chat.Format != FormatChat || chat.Auth != "bearer" {
		t.Errorf("format/auth = %q/%q", chat.Format, chat.Auth)
	}
	if chat.KeyEnv != "COMMAND_CODE_API_KEY" {
		t.Errorf("key_env = %q", chat.KeyEnv)
	}
	if len(chat.Models) < 80 {
		t.Errorf("models = %d, want the chat-capable list", len(chat.Models))
	}
	if !containsString(chat.Models, "deepseek/deepseek-v4-flash") {
		t.Error("a known chat model is missing")
	}
	for _, id := range []string{
		"Qwen/Qwen4-Max",
		"tencent/hy4-flash",
		"recursive/flint-v1.0:free",
	} {
		if !containsString(chat.Models, id) {
			t.Errorf("new upstream chat model %q is missing", id)
		}
	}
	for _, id := range chat.Models {
		if strings.HasPrefix(id, "claude-") {
			t.Errorf("chat entry advertises Claude model %q, which only answers on /messages", id)
		}
		if chat.Contexts[id] == 0 {
			t.Errorf("chat model %q has no context hint", id)
		}
	}

	// Claude models live on the Anthropic Messages surface; lgllm cannot send
	// them to /chat/completions, so they get their own catalog entry.
	claude, ok := CatalogByRef("cmdc")
	if !ok {
		t.Fatal("commandcode-claude is missing from the catalog")
	}
	if claude.ID != "commandcode-claude" || claude.Format != FormatAnthropic {
		t.Errorf("id/format = %q/%q", claude.ID, claude.Format)
	}
	if claude.BaseURL != chat.BaseURL || claude.KeyEnv != chat.KeyEnv {
		t.Errorf("Claude entry does not share the base URL and key env")
	}
	if len(claude.Models) == 0 {
		t.Error("Claude entry has no model hints")
	}
	for _, id := range claude.Models {
		if !strings.HasPrefix(id, "claude-") {
			t.Errorf("Claude entry advertises non-Claude model %q", id)
		}
		if claude.Contexts[id] == 0 {
			t.Errorf("Claude model %q has no context hint", id)
		}
	}

	cfg := config.Default()
	cfg.Connections = []config.Connection{
		{ID: "cmd-main", Provider: "cmd", APIKey: "cmd-key"},
		{ID: "cmdc-main", Provider: "cmdc", APIKey: "cmd-key"},
	}
	got := Materialize(cfg.Providers, cfg.Connections)
	if len(got) != 2 {
		t.Fatalf("materialized = %+v", got)
	}
	byID := make(map[string]config.Provider, len(got))
	for _, p := range got {
		byID[p.ID] = p
	}
	if byID["commandcode"].Format != FormatChat {
		t.Errorf("materialized commandcode format = %q", byID["commandcode"].Format)
	}
	if byID["commandcode-claude"].Format != FormatAnthropic {
		t.Errorf("materialized Claude format = %q", byID["commandcode-claude"].Format)
	}

	// A vendor-prefixed model keeps its slash after the provider prefix.
	reg, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, model, err := reg.Resolve("cmd/deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("Resolve chat: %v", err)
	}
	if p.ID != "commandcode" || model != "deepseek/deepseek-v4-flash" {
		t.Errorf("chat route = %s/%s", p.ID, model)
	}
	p, model, err = reg.Resolve("cmdc/claude-sonnet-5-5")
	if err != nil {
		t.Fatalf("Resolve Claude: %v", err)
	}
	if p.ID != "commandcode-claude" || model != "claude-sonnet-5-5" {
		t.Errorf("Claude route = %s/%s", p.ID, model)
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestCatalogContextHintsPropagate(t *testing.T) {
	cfg := config.Default()
	cfg.Connections = []config.Connection{
		{ID: "cmd", Provider: "cmd", APIKey: "key"},
		{ID: "cmdc", Provider: "cmdc", APIKey: "key"},
		{ID: "ocg", Provider: "ocg", APIKey: "key"},
	}
	effective := Materialize(cfg.Providers, cfg.Connections)
	find := func(providerID, modelID string) int {
		for _, provider := range effective {
			if provider.ID != providerID {
				continue
			}
			for _, model := range provider.Models {
				if model.ID == modelID {
					return model.Context
				}
			}
		}
		return 0
	}
	if got := find("commandcode", "deepseek/deepseek-v4.1-flash"); got != 1_000_000 {
		t.Errorf("commandcode context = %d", got)
	}
	if got := find("commandcode", "tencent/hy4-flash"); got != 1_024_000 {
		t.Errorf("commandcode hy4-flash context = %d, want the upstream value", got)
	}
	if got := find("commandcode", "Qwen/Qwen4-Max"); got != 1_000_000 {
		t.Errorf("commandcode Qwen4-Max context = %d", got)
	}
	if got := find("commandcode", "recursive/flint-v1.0:free"); got != 262_144 {
		t.Errorf("commandcode flint context = %d", got)
	}
	if got := find("commandcode-claude", "claude-sonnet-5-5"); got != 1_000_000 {
		t.Errorf("commandcode-claude context = %d", got)
	}
	if got := find("commandcode-claude", "claude-haiku-4-5-20251001"); got != 200_000 {
		t.Errorf("commandcode-claude haiku context = %d", got)
	}
	if got := find("opencode-go", "deepseek-v4.1-flash"); got != 1_000_000 {
		t.Errorf("opencode-go context = %d", got)
	}
}

func TestValidateSessionHeader(t *testing.T) {
	base := func() config.Config {
		cfg := config.Default()
		cfg.Providers = []config.Provider{{
			ID: "gw", BaseURL: "https://gw.example/v1", SessionHeader: "x-session-id",
		}}
		cfg.Connections = []config.Connection{{ID: "c", Provider: "gw", APIKey: "k"}}
		return cfg
	}

	cfg := base()
	if err := Validate(&cfg); err != nil {
		t.Fatalf("valid session_header rejected: %v", err)
	}

	for _, name := range []string{"bad header", "authorization", "content-length"} {
		cfg := base()
		cfg.Providers[0].SessionHeader = name
		if err := Validate(&cfg); err == nil {
			t.Errorf("Validate accepted session_header %q", name)
		}
	}
}

func TestMaterializeOverrideWins(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers = []config.Provider{{
		ID:      "deepseek",
		BaseURL: "https://gateway.internal/ds/v1",
		Headers: map[string]string{"x-team": "infra"},
	}}
	got := Materialize(cfg.Providers, cfg.Connections)

	if len(got) != 1 {
		t.Fatalf("got %d providers", len(got))
	}
	p := got[0]
	if p.BaseURL != "https://gateway.internal/ds/v1" {
		t.Errorf("base_url = %q", p.BaseURL)
	}
	// Unset fields still inherit from the catalog.
	if p.Format != FormatChat || p.Auth != "bearer" {
		t.Errorf("format/auth = %q/%q, catalog defaults were lost", p.Format, p.Auth)
	}
	if p.Headers["x-team"] != "infra" {
		t.Errorf("headers = %v", p.Headers)
	}
}

func TestMaterializeAliasDoesNotDuplicate(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers = []config.Provider{{ID: "ds", BaseURL: "https://mirror.internal/v1"}}
	got := Materialize(cfg.Providers, cfg.Connections)

	if len(got) != 1 {
		t.Fatalf("alias produced %d providers: %+v", len(got), got)
	}
	if got[0].ID != "deepseek" {
		t.Errorf("id = %q, want the canonical catalog id", got[0].ID)
	}
	if got[0].BaseURL != "https://mirror.internal/v1" {
		t.Errorf("override applied to %q", got[0].ID)
	}
}

func TestMaterializeCustomProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.Provider{{
		ID:      "gw",
		BaseURL: "https://gw.example/v1",
		Models:  []config.Model{{ID: "m1"}},
	}}
	cfg.Connections = []config.Connection{{ID: "c", Provider: "gw", APIKey: "k"}}
	got := Materialize(cfg.Providers, cfg.Connections)

	if len(got) != 1 {
		t.Fatalf("got %d providers", len(got))
	}
	if got[0].Format != FormatChat || got[0].Auth != "bearer" {
		t.Errorf("custom provider defaults missing: %q/%q", got[0].Format, got[0].Auth)
	}
}

func TestValidateCatchesCrossReferenceErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{
			name: "connection names an unknown provider",
			mutate: func(c *config.Config) {
				c.Connections = []config.Connection{{ID: "x", Provider: "ghost", APIKey: "k"}}
			},
			want: "unknown provider",
		},
		{
			name: "credentialed provider has no key",
			mutate: func(c *config.Config) {
				c.Connections = []config.Connection{{ID: "x", Provider: "deepseek"}}
			},
			want: "api_key",
		},
		{
			name: "default provider does not exist",
			mutate: func(c *config.Config) {
				c.DefaultProvider = "ghost"
			},
			want: "default_provider",
		},
		{
			name: "custom provider without base_url",
			mutate: func(c *config.Config) {
				c.Providers = []config.Provider{{ID: "gw"}}
				c.Connections = []config.Connection{{ID: "x", Provider: "gw", APIKey: "k"}}
			},
			want: "no base_url",
		},
		{
			name: "query auth without a parameter name",
			mutate: func(c *config.Config) {
				c.Providers = []config.Provider{{
					ID: "gw", BaseURL: "https://gw.example/v1", Auth: "query",
				}}
				c.Connections = []config.Connection{{ID: "x", Provider: "gw", APIKey: "k"}}
			},
			want: "auth_param",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mutate(&cfg)
			err := Validate(&cfg)
			if err == nil {
				t.Fatal("Validate accepted an invalid config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateAllowsKeylessLocalProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Connections = []config.Connection{{ID: "local", Provider: "ollama"}}
	if err := Validate(&cfg); err != nil {
		t.Fatalf("a provider with auth=none must not require a key: %v", err)
	}
}

func TestNewExcludesProvidersWithoutCredentials(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers = []config.Provider{{ID: "groq"}}

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Len() != 1 {
		t.Fatalf("usable providers = %d, want 1", r.Len())
	}
	if _, ok := r.Provider("groq"); ok {
		t.Error("groq has no connection and must not be routable")
	}
}

func TestResolve(t *testing.T) {
	cfg := baseConfig()
	cfg.Connections = append(cfg.Connections, config.Connection{
		ID: "or-main", Provider: "openrouter", APIKey: "sk-or",
	})
	cfg.DefaultProvider = "deepseek"
	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		model    string
		wantProv string
		wantUp   string
	}{
		{"deepseek/deepseek-chat", "deepseek", "deepseek-chat"},
		{"ds/deepseek-reasoner", "deepseek", "deepseek-reasoner"},
		// An unknown prefix is part of the model id (openrouter style), so the
		// default provider gets the whole string.
		{"deepseek-ai/DeepSeek-V3", "deepseek", "deepseek-ai/DeepSeek-V3"},
	}
	for _, tc := range tests {
		p, model, err := r.Resolve(tc.model)
		if err != nil {
			t.Errorf("Resolve(%q): %v", tc.model, err)
			continue
		}
		if p.ID != tc.wantProv || model != tc.wantUp {
			t.Errorf("Resolve(%q) = %s/%s, want %s/%s",
				tc.model, p.ID, model, tc.wantProv, tc.wantUp)
		}
	}
}

func TestResolveDefaultProvider(t *testing.T) {
	cfg := baseConfig()
	cfg.Connections = append(cfg.Connections, config.Connection{
		ID: "or-main", Provider: "openrouter", APIKey: "sk-or",
	})
	cfg.DefaultProvider = "openrouter"
	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	p, model, err := r.Resolve("google/gemini-2.5-pro")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "openrouter" {
		t.Fatalf("provider = %q, want the configured default", p.ID)
	}
	if model != "google/gemini-2.5-pro" {
		t.Errorf("model = %q", model)
	}
}

func TestResolveBareModelWithoutDefaultFails(t *testing.T) {
	cfg := baseConfig()
	cfg.Connections = append(cfg.Connections, config.Connection{
		ID: "or-main", Provider: "openrouter", APIKey: "sk-or",
	})
	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = r.Resolve("gpt-4o-mini")
	if err == nil {
		t.Fatal("expected an error when no default can be inferred")
	}
	if !strings.Contains(err.Error(), "provider/model") {
		t.Errorf("error should suggest the prefix form: %v", err)
	}
	// An explicit prefix always works.
	if _, _, err := r.Resolve("openrouter/gpt-4o-mini"); err != nil {
		t.Fatalf("prefixed resolve failed: %v", err)
	}
}

func TestResolveInfersSoleProvider(t *testing.T) {
	cfg := baseConfig()
	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, model, err := r.Resolve("anything")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "deepseek" || model != "anything" {
		t.Errorf("got %s/%s", p.ID, model)
	}
}

func TestResolveRejectsEmptyModel(t *testing.T) {
	cfg := baseConfig()
	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Resolve("   "); err == nil {
		t.Fatal("an empty model must be rejected")
	}
}

func TestConnectionBaseURLOverride(t *testing.T) {
	cfg := baseConfig()
	cfg.Connections[0].BaseURL = "http://127.0.0.1:9999/v1"

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := r.Provider("deepseek")
	if !ok {
		t.Fatal("provider missing")
	}
	conn, err := p.Connection()
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := p.Endpoint(conn)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "http://127.0.0.1:9999/v1" {
		t.Errorf("endpoint = %q, want the connection override", endpoint)
	}
}

func TestSupportsPassthrough(t *testing.T) {
	cases := map[string]bool{
		FormatChat:      true,
		FormatAnthropic: false,
		FormatGemini:    false,
		FormatResponses: false,
		FormatOllama:    false,
	}
	for format, want := range cases {
		if got := (&Provider{Format: format}).SupportsPassthrough(); got != want {
			t.Errorf("format %q: SupportsPassthrough = %v, want %v", format, got, want)
		}
	}
}
