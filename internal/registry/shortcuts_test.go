package registry

import (
	"strings"
	"testing"

	"lgllm/internal/config"
)

// shortcutConfig has two custom providers with known model lists, so a test can
// tell which member a combo actually chose.
func shortcutConfig() config.Config {
	cfg := config.Default()
	cfg.Auth.Mode = "off"
	cfg.Providers = []config.Provider{
		{
			ID: "acme", BaseURL: "https://acme.example/v1", Format: FormatChat,
			Auth:   "bearer",
			Models: []config.Model{{ID: "acme-small"}, {ID: "acme-large"}},
		},
		{
			ID: "other", BaseURL: "https://other.example/v1", Format: FormatChat,
			Auth:   "bearer",
			Models: []config.Model{{ID: "other-one"}},
		},
	}
	cfg.Connections = []config.Connection{
		{ID: "acme-main", Provider: "acme", APIKey: "sk-acme"},
		{ID: "other-main", Provider: "other", APIKey: "sk-other"},
	}
	return cfg
}

func TestResolveAlias(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Aliases = map[string]string{"fast": "acme/acme-small"}

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, model, err := r.Resolve("fast")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "acme" || model != "acme-small" {
		t.Errorf("resolved %s/%s, want acme/acme-small", p.ID, model)
	}
}

func TestResolveAliasChain(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Aliases = map[string]string{
		"fast":     "quick",
		"quick":    "acme/acme-small",
		"provider": "acme/", // provider default model
	}

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, model, err := r.Resolve("fast"); err != nil || model != "acme-small" {
		t.Errorf("chained alias: model = %q, err = %v", model, err)
	}
	if _, model, err := r.Resolve("provider"); err != nil || model != "acme-small" {
		t.Errorf("provider default: model = %q, err = %v", model, err)
	}
}

func TestValidateRejectsAliasCycle(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Aliases = map[string]string{"a": "b", "b": "a"}

	err := Validate(&cfg)
	if err == nil {
		t.Fatal("a cycle must be refused")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want it to mention the cycle", err)
	}
}

func TestValidateRejectsAliasToUnknownProvider(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Aliases = map[string]string{"fast": "ghost/gpt"}

	err := Validate(&cfg)
	if err == nil {
		t.Fatal("an unknown provider must be refused")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %v, want the provider named", err)
	}
}

func TestValidateRejectsBareAliasTarget(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Aliases = map[string]string{"fast": "acme-small"}

	err := Validate(&cfg)
	if err == nil {
		t.Fatal("a bare model target must be refused")
	}
	if !strings.Contains(err.Error(), "provider/model") {
		t.Errorf("error = %v, want the expected form explained", err)
	}
}

func TestValidateRejectsShortcutNameCollision(t *testing.T) {
	t.Run("catalog provider alias", func(t *testing.T) {
		cfg := baseConfig()
		// "ds" is deepseek's catalog alias; config never declares it.
		cfg.Aliases = map[string]string{"ds": "deepseek/deepseek-chat"}

		err := Validate(&cfg)
		if err == nil {
			t.Fatal("a catalog alias must not be reused as a model alias")
		}
		if !strings.Contains(err.Error(), "collides") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("combo and alias", func(t *testing.T) {
		cfg := shortcutConfig()
		cfg.Aliases = map[string]string{"fast": "acme/acme-small"}
		cfg.Combos = []config.Combo{{Name: "fast", Members: []string{"acme/acme-small"}}}

		if err := Validate(&cfg); err == nil {
			t.Fatal("one name may not be both an alias and a combo")
		}
	})
}

func TestResolvePrefixInference(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Connections = append(cfg.Connections,
		config.Connection{ID: "oa-main", Provider: "openai", APIKey: "sk-oa"},
		config.Connection{ID: "cc-main", Provider: "anthropic", APIKey: "sk-cc"},
	)
	cfg.DefaultProvider = "acme"

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"gpt-4o-mini":            "openai",
		"o3-mini":                "openai",
		"text-embedding-3-small": "openai",
		"claude-sonnet-4":        "anthropic",
	}
	for model, want := range cases {
		p, got, err := r.Resolve(model)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if p.ID != want || got != model {
			t.Errorf("%s resolved to %s/%s, want %s/%s", model, p.ID, got, want, model)
		}
	}

	// A prefix whose provider has no credential must not hijack the default.
	cfg = shortcutConfig()
	cfg.DefaultProvider = "acme"
	r, err = New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, model, err := r.Resolve("gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "acme" || model != "gpt-4o-mini" {
		t.Errorf("keyless inference gave %s/%s, want the default provider", p.ID, model)
	}

	// Ambiguous vendor prefixes are left alone on purpose.
	if p, _, err := r.Resolve("llama-3.3-70b-versatile"); err != nil || p.ID != "acme" {
		t.Errorf("llama prefix must not be inferred: %v", err)
	}
}

func TestResolveVendorSlashIsNotInferred(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Connections = append(cfg.Connections,
		config.Connection{ID: "ds-main", Provider: "deepseek", APIKey: "sk-ds"},
	)
	cfg.DefaultProvider = "acme"

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	// "deepseek-ai/..." is an OpenRouter-style id, not a provider prefix.
	p, model, err := r.Resolve("deepseek-ai/DeepSeek-V3")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "acme" || model != "deepseek-ai/DeepSeek-V3" {
		t.Errorf("got %s/%s, want the default provider and the full id", p.ID, model)
	}
}

func TestResolveComboSkipsUnusableMember(t *testing.T) {
	cfg := shortcutConfig()
	// "keyless" is declared but has no connection, so New drops it.
	cfg.Providers = append(cfg.Providers, config.Provider{
		ID: "keyless", BaseURL: "https://keyless.example/v1", Format: FormatChat,
		Auth:   "bearer",
		Models: []config.Model{{ID: "keyless-model"}},
	})
	cfg.Combos = []config.Combo{{
		Name:    "coding",
		Members: []string{"keyless/keyless-model", "other/other-one"},
	}}

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, model, err := r.Resolve("coding")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "other" || model != "other-one" {
		t.Errorf("combo chose %s/%s, want the usable member", p.ID, model)
	}
}

func TestResolveComboWithoutUsableMemberFails(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Providers = append(cfg.Providers, config.Provider{
		ID: "keyless", BaseURL: "https://keyless.example/v1", Format: FormatChat,
		Auth: "bearer",
	})
	cfg.Combos = []config.Combo{{
		Name:    "coding",
		Members: []string{"keyless/anything"},
	}}

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Resolve("coding"); err == nil {
		t.Fatal("a combo with no usable member must fail")
	}
}

func TestResolveComboRoundRobinRotates(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Combos = []config.Combo{{
		Name:     "coding",
		Strategy: StrategyRoundRobin,
		Members:  []string{"acme/acme-small", "other/other-one"},
	}}

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		p, model, err := r.Resolve("coding")
		if err != nil {
			t.Fatal(err)
		}
		seen[p.ID+"/"+model] = true
	}
	if !seen["acme/acme-small"] || !seen["other/other-one"] {
		t.Errorf("round-robin did not visit both members: %v", seen)
	}
}

func TestValidateRejectsNestedCombo(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Combos = []config.Combo{
		{Name: "inner", Members: []string{"acme/acme-small"}},
		{Name: "outer", Members: []string{"inner"}},
	}

	err := Validate(&cfg)
	if err == nil {
		t.Fatal("a combo member may not name another combo")
	}
	if !strings.Contains(err.Error(), "nest") {
		t.Errorf("error = %v", err)
	}
}

func TestAliasesAndCombosListing(t *testing.T) {
	cfg := shortcutConfig()
	// Declared but unconnected: New drops the provider, so the alias and combo
	// that depend on it disappear from the listing.
	cfg.Providers = append(cfg.Providers, config.Provider{
		ID: "keyless", BaseURL: "https://keyless.example/v1", Format: FormatChat,
		Auth:   "bearer",
		Models: []config.Model{{ID: "keyless-model"}},
	})
	cfg.Aliases = map[string]string{
		"fast":  "acme/acme-small",
		"broke": "keyless/keyless-model",
	}
	cfg.Combos = []config.Combo{
		{Name: "coding", Members: []string{"acme/acme-large", "other/other-one"}},
		{Name: "dead", Members: []string{"keyless/keyless-model"}},
	}

	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	aliases := r.Aliases()
	if len(aliases) != 1 || aliases[0].Name != "fast" {
		t.Fatalf("aliases = %+v, want only the usable one", aliases)
	}
	if aliases[0].Target.Provider.ID != "acme" || aliases[0].Target.Model != "acme-small" {
		t.Errorf("alias target = %+v", aliases[0].Target)
	}

	combos := r.Combos()
	if len(combos) != 1 || combos[0].Name != "coding" {
		t.Fatalf("combos = %+v, want only the usable one", combos)
	}
	if len(combos[0].Members) != 2 {
		t.Errorf("members = %+v", combos[0].Members)
	}

	// Shortcut reports declared names only, so a bare model never looks like a
	// configured alias to the discovery endpoints.
	if _, ok := r.Shortcut("acme-small"); ok {
		t.Error("an advertised model must not report as a shortcut")
	}
	if tgt, ok := r.Shortcut("fast"); !ok || tgt.Model != "acme-small" {
		t.Errorf("Shortcut(fast) = %+v, %v", tgt, ok)
	}
}

func TestPlanListsComboMembersInOrder(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Combos = []config.Combo{{
		Name:    "coding",
		Members: []string{"acme/acme-small", "other/other-one"},
	}}
	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := r.Plan("coding")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Combo != "coding" || len(plan.Attempts) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Attempts[0].Provider.ID != "acme" || plan.Attempts[1].Provider.ID != "other" {
		t.Errorf("attempt order = %+v", plan.Attempts)
	}

	// A plain model still yields exactly one attempt.
	single, err := r.Plan("acme/acme-large")
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Attempts) != 1 || single.Attempts[0].Model != "acme-large" {
		t.Errorf("single plan = %+v", single)
	}
}

func TestPlanRoundRobinRotatesStartingMember(t *testing.T) {
	cfg := shortcutConfig()
	cfg.Combos = []config.Combo{{
		Name:     "coding",
		Strategy: StrategyRoundRobin,
		Members:  []string{"acme/acme-small", "other/other-one"},
	}}
	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	first, _ := r.Plan("coding")
	second, _ := r.Plan("coding")
	if first.Attempts[0].Provider.ID == second.Attempts[0].Provider.ID {
		t.Errorf("round-robin did not rotate: %s then %s",
			first.Attempts[0].Provider.ID, second.Attempts[0].Provider.ID)
	}
}

func TestPlanCarriesComboTokenSaverOptOut(t *testing.T) {
	off := false
	cfg := shortcutConfig()
	cfg.Combos = []config.Combo{{
		Name:       "coding",
		Members:    []string{"acme/acme-small"},
		TokenSaver: &off,
	}}
	r, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Plan("coding")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.TokenSaverOff {
		t.Error("the combo opt-out did not reach the plan")
	}

	// A plain model keeps the default (saver on).
	plain, err := r.Plan("acme/acme-small")
	if err != nil {
		t.Fatal(err)
	}
	if plain.TokenSaverOff {
		t.Error("a plain target must not opt out")
	}
}
