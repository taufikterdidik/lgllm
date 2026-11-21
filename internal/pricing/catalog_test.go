package pricing

import (
	"math"
	"testing"

	"lgllm/internal/registry"
)

func TestModelRateForAliases(t *testing.T) {
	for _, provider := range []string{"opencode-go", "ocg", "opencode-go-anthropic", "ocga"} {
		rate, ok := ModelRateFor(provider, "qwen4-max")
		if !ok {
			t.Fatalf("%s: rate not found", provider)
		}
		if rate.Input != 1.5 || rate.Output != 4.5 || rate.CacheRead != 0.075 || rate.CacheWrite != 1.875 {
			t.Errorf("%s: rate = %+v", provider, rate)
		}
	}

	for _, provider := range []string{"commandcode", "cmd", "commandcode-claude", "cmdc"} {
		rate, ok := ModelRateFor(provider, "claude-sonnet-5-5")
		if !ok {
			t.Fatalf("%s: rate not found", provider)
		}
		if rate.Input != 2 || rate.Output != 10 || rate.CacheRead != 0.1 || rate.CacheWrite != 2.5 {
			t.Errorf("%s: rate = %+v", provider, rate)
		}
	}
}

func TestModelRateForSystemOneAliases(t *testing.T) {
	rate, ok := ModelRateFor("ocs", "jev-1.13-free")
	if !ok || rate.Input != 0 {
		t.Errorf("ocga free jev = %+v, %v", rate, ok)
	}
	rate, ok = ModelRateFor("cmds", "typesafe/jev")
	if !ok || rate.Input != 0.042 {
		t.Errorf("cmds jev = %+v, %v", rate, ok)
	}
}

func TestModelRateForFreeAndUnknown(t *testing.T) {
	rate, ok := ModelRateFor("commandcode", "recursive/flint-v1.0:free")
	if !ok {
		t.Fatal("free model reported no rate")
	}
	if rate.Input != 0 || rate.Output != 0 || rate.CacheRead != 0 {
		t.Errorf("free rate = %+v, want zeros", rate)
	}
	if _, ok := ModelRateFor("commandcode", "my-private-finetune"); ok {
		t.Error("unknown model reported a rate")
	}
	if _, ok := ModelRateFor("custom-provider", "gpt-4o"); ok {
		t.Error("unknown provider reported a rate")
	}
}

func TestEstimateForPrefersProviderCatalog(t *testing.T) {
	// qwen4-max: 1.5 input / 4.5 output per 1M tokens.
	cost, known := EstimateFor("opencode-go", "qwen4-max", 1_000_000, 1_000_000)
	if !known || math.Abs(cost-6) > 1e-9 {
		t.Errorf("opencode-go cost = %v, known = %v", cost, known)
	}
	cost, known = EstimateFor("cmdc", "claude-sonnet-5-5", 1_000_000, 1_000_000)
	if !known || math.Abs(cost-12) > 1e-9 {
		t.Errorf("commandcode cost = %v, known = %v", cost, known)
	}
	// A built-in provider with an unknown model must not borrow the generic
	// OpenAI table: its real price is unpublished, not gpt-4o-mini's.
	if _, ok := EstimateFor("opencode-go", "gpt-4o-mini", 1000, 1000); ok {
		t.Error("a built-in provider fell back to the generic table")
	}
	// Custom providers keep the old model-only fallback.
	cost, known = EstimateFor("my-gateway", "gpt-4o-mini", 1_000_000, 1_000_000)
	if !known || math.Abs(cost-0.75) > 1e-9 {
		t.Errorf("custom provider cost = %v, known = %v", cost, known)
	}
}

// TestCatalogCoverage keeps the display table in sync with the advertised
// models. A new catalog entry without a price would otherwise show "unknown"
// in the dashboard until someone notices.
func TestCatalogCoverage(t *testing.T) {
	for _, provider := range []string{"opencode-go", "commandcode", "commandcode-claude"} {
		b, ok := registry.CatalogByRef(provider)
		if !ok {
			t.Fatalf("catalog provider %q is missing", provider)
		}
		for _, model := range b.Models {
			if _, ok := ModelRateFor(provider, model); !ok {
				t.Errorf("%s/%s has no display rate", provider, model)
			}
		}
	}
	for _, provider := range []string{"commandcode-systemone", "opencode-systemone"} {
		b, ok := registry.CatalogByRef(provider)
		if !ok {
			t.Fatalf("catalog provider %q is missing", provider)
		}
		for _, model := range b.Models {
			if _, ok := ModelRateFor(provider, model); !ok {
				t.Errorf("%s/%s has no display rate", provider, model)
			}
		}
	}
}
