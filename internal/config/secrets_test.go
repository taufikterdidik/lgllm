package config

import (
	"strings"
	"testing"

	"lgllm/internal/secret"
)

func TestDecryptSecrets(t *testing.T) {
	box, err := secret.New("test-master-key-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	connKey, _ := box.Encrypt("sk-provider")
	authKey, _ := box.Encrypt("sk-client")
	header, _ := box.Encrypt("header-secret")

	cfg, err := Parse([]byte(shortcutBase + `
auth:
  mode: required
  keys:
    - ` + authKey + `
providers:
  - id: p
    base_url: https://p.example/v1
    headers:
      x-api-key: ` + header + `
connections:
  - id: c
    provider: p
    api_key: ` + connKey + `
`))
	if err != nil {
		t.Fatal(err)
	}

	if err := DecryptSecrets(cfg, "test-master-key-long-enough"); err != nil {
		t.Fatal(err)
	}
	if cfg.Connections[0].APIKey != "sk-provider" {
		t.Errorf("connection key = %q", cfg.Connections[0].APIKey)
	}
	if cfg.Auth.Keys[0] != "sk-client" {
		t.Errorf("auth key = %q", cfg.Auth.Keys[0])
	}
	if cfg.Providers[0].Headers["x-api-key"] != "header-secret" {
		t.Errorf("header = %q", cfg.Providers[0].Headers["x-api-key"])
	}
}

func TestDecryptSecretsWithoutKeyFails(t *testing.T) {
	box, _ := secret.New("test-master-key-long-enough")
	enc, _ := box.Encrypt("sk-provider")
	cfg, err := Parse([]byte(shortcutBase + `
providers:
  - id: p
    base_url: https://p.example/v1
connections:
  - id: c
    provider: p
    api_key: ` + enc + `
`))
	if err != nil {
		t.Fatal(err)
	}

	err = DecryptSecrets(cfg, "")
	if err == nil {
		t.Fatal("an encrypted value without a key must fail")
	}
	if !strings.Contains(err.Error(), "master key") || !strings.Contains(err.Error(), "connections[0]") {
		t.Errorf("error = %v, want it to name the key and the field", err)
	}
}

func TestDecryptLeavesPlaintextAlone(t *testing.T) {
	cfg, err := Parse([]byte(shortcutBase + `
providers:
  - id: p
    base_url: https://p.example/v1
connections:
  - id: c
    provider: p
    api_key: sk-plain
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := DecryptSecrets(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Connections[0].APIKey != "sk-plain" {
		t.Errorf("key = %q", cfg.Connections[0].APIKey)
	}
}

func TestValidateRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"negative rpm", "auth:\n  rate_limit:\n    requests_per_minute: -1\n", "requests_per_minute"},
		{"negative burst", "auth:\n  rate_limit:\n    burst: -1\n", "burst"},
		{"negative quota", "auth:\n  rate_limit:\n    tokens_per_day: -1\n", "tokens_per_day"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(shortcutBase + tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
