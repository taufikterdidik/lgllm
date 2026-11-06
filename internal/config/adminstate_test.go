package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lgllm/internal/secret"
)

func TestSaveAndLoadAdminState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.yaml")
	st := &AdminState{
		Providers: []Provider{{
			ID: "p", BaseURL: "https://p.example/v1", Format: "openai", Auth: "bearer",
			Models: []Model{{ID: "m"}},
		}},
		Connections: []Connection{{ID: "c", Provider: "p", APIKey: "sk-secret"}},
		Combos:      []Combo{{Name: "combo", Members: []string{"p/m"}}},
		Keys:        []string{"sk-admin-key"},
	}
	if err := SaveAdminState(path, st, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}

	got, err := LoadAdminState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 || got.Providers[0].ID != "p" {
		t.Errorf("providers = %+v", got.Providers)
	}
	if got.Connections[0].APIKey != "sk-secret" {
		t.Errorf("key = %q", got.Connections[0].APIKey)
	}
}

func TestSaveAdminStateEncrypts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.yaml")
	box, err := secret.New("test-master-key-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	st := &AdminState{
		Connections: []Connection{{ID: "c", Provider: "p", APIKey: "sk-secret"}},
		Keys:        []string{"sk-admin-key"},
		Providers: []Provider{{
			ID: "p", Headers: map[string]string{"x-api-key": "header-secret"},
		}},
	}
	if err := SaveAdminState(path, st, box); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "sk-secret") || strings.Contains(string(raw), "header-secret") {
		t.Fatalf("plaintext secret on disk:\n%s", raw)
	}
	if !strings.Contains(string(raw), "enc:v1:") {
		t.Errorf("no ciphertext in:\n%s", raw)
	}

	got, err := LoadAdminState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !secret.IsEncrypted(got.Connections[0].APIKey) {
		t.Error("connection key is not encrypted")
	}
	if !secret.IsEncrypted(got.Providers[0].Headers["x-api-key"]) {
		t.Error("header is not encrypted")
	}
}

func TestLoadAdminStateMissingIsEmpty(t *testing.T) {
	st, err := LoadAdminState(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Providers)+len(st.Connections)+len(st.Combos)+len(st.Keys) != 0 {
		t.Errorf("state = %+v", st)
	}
}

func TestMergeAdminStateRejectsDuplicates(t *testing.T) {
	cfg, err := Parse([]byte(shortcutBase + `
providers:
  - id: p
    base_url: https://p.example/v1
    format: openai
connections:
  - {id: c, provider: p}
combos:
  - {name: combo, members: [p/m]}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []*AdminState{
		{Providers: []Provider{{ID: "p"}}},
		{Connections: []Connection{{ID: "c"}}},
		{Combos: []Combo{{Name: "combo"}}},
	} {
		if err := MergeAdminState(cfg, st); err == nil {
			t.Errorf("duplicate accepted: %+v", st)
		}
	}
}

func TestMergeAdminStateAppends(t *testing.T) {
	cfg, err := Parse([]byte(shortcutBase + `
providers:
  - id: base
    base_url: https://base.example/v1
    format: openai
`))
	if err != nil {
		t.Fatal(err)
	}
	st := &AdminState{
		Providers:   []Provider{{ID: "extra", BaseURL: "https://extra.example/v1", Format: "openai"}},
		Connections: []Connection{{ID: "extra-c", Provider: "extra"}},
		Combos:      []Combo{{Name: "extra-combo", Members: []string{"extra/m"}}},
		Keys:        []string{"admin-key-1"},
	}
	if err := MergeAdminState(cfg, st); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 2 || len(cfg.Connections) != 1 || len(cfg.Combos) != 1 {
		t.Errorf("merged config = %+v", cfg)
	}
	if len(cfg.Auth.Keys) != 1 {
		t.Errorf("keys = %v", cfg.Auth.Keys)
	}
}
