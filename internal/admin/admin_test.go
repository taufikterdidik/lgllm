package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lgllm/internal/config"
	"lgllm/internal/registry"
)

func writeBase(t *testing.T, withAdmin bool) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	base = filepath.Join(dir, "config.yaml")
	adminBlock := ""
	if withAdmin {
		adminBlock = "admin:\n  enabled: true\n  token: sk-admin-token-123\n"
	}
	body := `server:
  listen: 127.0.0.1:39290
auth:
  mode: off
` + adminBlock + `providers:
  - id: base
    base_url: https://base.example/v1
    format: openai
    auth: bearer
    models: [{id: base-model}]
connections:
  - {id: base-conn, provider: base, api_key: sk-base}
`
	if err := os.WriteFile(base, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, base
}

func newManager(t *testing.T, dir, base string, apply func(*config.Config) error) *Manager {
	t.Helper()
	m, err := New(Options{
		BasePath:  base,
		StatePath: filepath.Join(dir, "admin.yaml"),
		Apply:     apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestProviderLifecycle(t *testing.T) {
	dir, base := writeBase(t, true)
	var applied int
	m := newManager(t, dir, base, func(c *config.Config) error {
		if _, err := registry.New(c); err != nil {
			return err
		}
		applied++
		return nil
	})

	err := m.AddProvider(config.Provider{
		ID: "extra", BaseURL: "https://extra.example/v1", Format: "openai", Auth: "bearer",
		Models: []config.Model{{ID: "extra-model"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Errorf("apply called %d times", applied)
	}
	if !m.HasProvider("extra") {
		t.Error("provider not reported as admin-managed")
	}
	state := m.State()
	if len(state.Providers) != 1 || state.Providers[0].ID != "extra" {
		t.Errorf("state = %+v", state)
	}

	// The merged config includes the admin provider.
	if len(m.Config().Providers) != 2 {
		t.Errorf("merged providers = %+v", m.Config().Providers)
	}

	// A connection can be added, but the provider cannot be deleted while it is
	// still referenced.
	if err := m.AddConnection(config.Connection{ID: "extra-conn", Provider: "extra", APIKey: "sk-extra"}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteProvider("extra"); err == nil {
		t.Fatal("deleting a referenced provider should fail")
	}
	if err := m.DeleteConnection("extra-conn"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteProvider("extra"); err != nil {
		t.Fatal(err)
	}
	if m.HasProvider("extra") {
		t.Error("provider still present after delete")
	}
}

func TestUpdateConnectionKeepsRedactedKey(t *testing.T) {
	dir, base := writeBase(t, true)
	m := newManager(t, dir, base, nil)
	if err := m.AddConnection(config.Connection{ID: "c", Provider: "base", APIKey: "sk-original"}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateConnection("c", config.Connection{ID: "c", Provider: "base", APIKey: "***", BaseURL: "https://x"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range m.State().Connections {
		if c.ID == "c" {
			if c.APIKey != "sk-original" {
				t.Errorf("api key = %q, want the preserved one", c.APIKey)
			}
			if c.BaseURL != "https://x" {
				t.Errorf("base url = %q", c.BaseURL)
			}
			return
		}
	}
	t.Fatal("connection not found")
}

func TestMutationRollsBackOnInvalidConfig(t *testing.T) {
	dir, base := writeBase(t, true)
	statePath := filepath.Join(dir, "admin.yaml")
	m := newManager(t, dir, base, func(c *config.Config) error {
		_, err := registry.New(c)
		return err
	})

	// A custom provider without base_url fails registry validation.
	err := m.AddProvider(config.Provider{ID: "broken", Format: "openai", Auth: "bearer"})
	if err == nil {
		t.Fatal("invalid provider was accepted")
	}
	if m.HasProvider("broken") {
		t.Error("failed mutation left state behind")
	}
	if _, statErr := os.Stat(statePath); statErr == nil {
		raw, _ := os.ReadFile(statePath)
		if strings.Contains(string(raw), "broken") {
			t.Errorf("failed mutation persisted:\n%s", raw)
		}
	}
}

func TestKeys(t *testing.T) {
	dir, base := writeBase(t, true)
	m := newManager(t, dir, base, nil)
	if err := m.AddKey("sk-admin-key-1"); err != nil {
		t.Fatal(err)
	}
	id := KeyID("sk-admin-key-1")
	if err := m.DeleteKey(id); err != nil {
		t.Fatal(err)
	}
	if len(m.State().Keys) != 0 {
		t.Error("key was not deleted")
	}
	if err := m.DeleteKey("nope"); err == nil {
		t.Error("deleting an unknown key should fail")
	}
}

func TestSecretsAreEncryptedOnDisk(t *testing.T) {
	dir, base := writeBase(t, true)
	m, err := New(Options{
		BasePath:  base,
		StatePath: filepath.Join(dir, "admin.yaml"),
		Overrides: config.Overrides{MasterKey: "test-master-key-long-enough"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddConnection(config.Connection{ID: "c", Provider: "base", APIKey: "sk-plain-secret"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "admin.yaml"))
	if strings.Contains(string(raw), "sk-plain-secret") {
		t.Fatalf("plaintext key on disk:\n%s", raw)
	}

	// A fresh manager decrypts the overlay so its in-memory state is usable.
	m2, err := New(Options{
		BasePath:  base,
		StatePath: filepath.Join(dir, "admin.yaml"),
		Overrides: config.Overrides{MasterKey: "test-master-key-long-enough"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.State().Connections[0].APIKey; got != "sk-plain-secret" {
		t.Errorf("decrypted key = %q", got)
	}
}
