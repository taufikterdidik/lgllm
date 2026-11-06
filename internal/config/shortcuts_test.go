package config

import (
	"strings"
	"testing"
)

const shortcutBase = "server:\n  listen: 127.0.0.1:20128\n"

func TestParseAliasesAndCombos(t *testing.T) {
	cfg, err := Parse([]byte(shortcutBase + `
aliases:
  " fast ": " ds/deepseek-chat "
  smart: "oa/gpt-4.1"
combos:
  - name: " coding "
    strategy: " Round-Robin "
    members:
      - "ds/deepseek-chat"
      - "cc/"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Aliases["fast"]; got != "ds/deepseek-chat" {
		t.Errorf("alias target = %q, want it trimmed", got)
	}
	if _, ok := cfg.Aliases[" fast "]; ok {
		t.Error("alias name was not trimmed")
	}
	if len(cfg.Combos) != 1 {
		t.Fatalf("got %d combos, want 1", len(cfg.Combos))
	}
	cb := cfg.Combos[0]
	if cb.Name != "coding" {
		t.Errorf("combo name = %q", cb.Name)
	}
	if cb.Strategy != "round-robin" {
		t.Errorf("strategy = %q, want it lower-cased", cb.Strategy)
	}
	if len(cb.Members) != 2 || cb.Members[1] != "cc/" {
		t.Errorf("members = %v", cb.Members)
	}
}

func TestParseRejectsBadShortcuts(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "alias name with a slash",
			yaml: "aliases:\n  ds/fast: ds/deepseek-chat\n",
			want: "must not contain",
		},
		{
			name: "alias with an empty target",
			yaml: "aliases:\n  fast: \"  \"\n",
			want: "target is required",
		},
		{
			name: "combo without a name",
			yaml: "combos:\n  - members: [ds/a]\n",
			want: "name is required",
		},
		{
			name: "duplicate combo names",
			yaml: "combos:\n  - name: coding\n    members: [ds/a]\n  - name: coding\n    members: [ds/b]\n",
			want: "duplicated",
		},
		{
			name: "unknown strategy",
			yaml: "combos:\n  - name: coding\n    strategy: fastest\n    members: [ds/a]\n",
			want: "priority|round-robin",
		},
		{
			name: "combo without members",
			yaml: "combos:\n  - name: coding\n    members: []\n",
			want: "at least one",
		},
		{
			name: "alias and combo share a name",
			yaml: "aliases:\n  coding: ds/a\ncombos:\n  - name: coding\n    members: [ds/a]\n",
			want: "already used by a combo",
		},
		{
			name: "alias shadows a declared provider id",
			yaml: "providers:\n  - id: ds\n    base_url: https://ds.example/v1\naliases:\n  ds: ds/m\n",
			want: "collides with a provider id",
		},
		{
			name: "alias shadows a declared provider alias",
			yaml: "providers:\n  - id: deepseek\n    alias: ds\n    base_url: https://ds.example/v1\naliases:\n  ds: deepseek/m\n",
			want: "collides with a provider alias",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(shortcutBase + tc.yaml))
			if err == nil {
				t.Fatal("Parse accepted an invalid shortcut")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseRejectsUnknownComboField(t *testing.T) {
	// A misspelled "members" would look like an empty combo; the strict decoder
	// is the only thing that catches it.
	_, err := Parse([]byte(shortcutBase + "combos:\n  - name: coding\n    member: [ds/a]\n"))
	if err == nil {
		t.Fatal("a misspelled combo field must fail")
	}
	if !strings.Contains(err.Error(), "member") {
		t.Errorf("error = %v, want the bad field named", err)
	}
}

func TestFingerprintCoversShortcuts(t *testing.T) {
	a, err := Parse([]byte(shortcutBase + "aliases:\n  fast: ds/a\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(shortcutBase + "aliases:\n  fast: ds/b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("a changed alias target must change the fingerprint")
	}
}

func TestParseTokenSaverOptions(t *testing.T) {
	cfg, err := Parse([]byte(shortcutBase + `
tokensaver:
  enabled: true
  min_bytes: 512
  max_bytes: 4096
  prompt: "answer tersely"
combos:
  - name: coding
    members: [ds/a]
    token_saver: false
  - name: plain
    members: [ds/b]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenSaver.Prompt != "answer tersely" {
		t.Errorf("prompt = %q", cfg.TokenSaver.Prompt)
	}
	if cfg.Combos[0].TokenSaver == nil || *cfg.Combos[0].TokenSaver {
		t.Errorf("combo opt-out = %v", cfg.Combos[0].TokenSaver)
	}
	if cfg.Combos[1].TokenSaver != nil {
		t.Errorf("unspecified token_saver = %v, want nil", cfg.Combos[1].TokenSaver)
	}
}
