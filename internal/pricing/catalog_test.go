package pricing

import (
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
