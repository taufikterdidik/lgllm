package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("server:\n  listen: 127.0.0.1:20128\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Auth.Mode != "localhost" {
		t.Errorf("auth.mode = %q, want localhost", cfg.Auth.Mode)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Errorf("log defaults = %q/%q", cfg.Log.Level, cfg.Log.Format)
	}
	if cfg.Server.ReadHeaderTimeout != Duration(10_000_000_000) {
		t.Errorf("read_header_timeout = %v", cfg.Server.ReadHeaderTimeout.Std())
	}
}

func TestParseEmptyIsAnError(t *testing.T) {
	if _, err := Parse([]byte("\n\n")); err == nil {
		t.Fatal("empty config should fail")
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte("server:\n  read_header_timout: 5s\n"))
	if err == nil {
		t.Fatal("a typo in a key must fail loudly")
	}
	if !strings.Contains(err.Error(), "read_header_timout") {
		t.Errorf("error should name the bad key, got: %v", err)
	}
}

func TestParseRejectsMultipleDocuments(t *testing.T) {
	_, err := Parse([]byte("log:\n  level: info\n---\nlog:\n  level: debug\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("want multiple-document error, got %v", err)
	}
}

func TestDurationFormats(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  listen: 127.0.0.1:20128
  read_header_timeout: 5s
  idle_timeout: 90
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Server.ReadHeaderTimeout.Std().Seconds(); got != 5 {
		t.Errorf("read_header_timeout = %vs, want 5s", got)
	}
	if got := cfg.Server.IdleTimeout.Std().Seconds(); got != 90 {
		t.Errorf("idle_timeout = %vs, want 90s (bare integer means seconds)", got)
	}
}

func TestValidateAuthPolicy(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "auth off on a public interface is refused",
			yaml:    "server:\n  listen: 0.0.0.0:20128\nauth:\n  mode: off\n",
			wantErr: "loopback",
		},
		{
			name: "auth off on loopback is allowed",
			yaml: "server:\n  listen: 127.0.0.1:20128\nauth:\n  mode: off\n",
		},
		{
			name:    "required without keys is refused",
			yaml:    "server:\n  listen: 127.0.0.1:20128\nauth:\n  mode: required\n",
			wantErr: "at least one key",
		},
		{
			name:    "required with a short key is refused",
			yaml:    "server:\n  listen: 127.0.0.1:20128\nauth:\n  mode: required\n  keys: [short]\n",
			wantErr: "shorter than 8",
		},
		{
			name: "required with a hashed key is allowed",
			yaml: "server:\n  listen: 0.0.0.0:20128\nauth:\n  mode: required\n  keys: [\"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"]\n",
		},
		{
			name:    "bad digest is reported",
			yaml:    "server:\n  listen: 127.0.0.1:20128\nauth:\n  mode: localhost\n  keys: [\"sha256:zz\"]\n",
			wantErr: "sha256",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateListen(t *testing.T) {
	for _, bad := range []string{"", "127.0.0.1", "127.0.0.1:70000", ":20128", "127.0.0.1:http"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := Parse([]byte("server:\n  listen: \"" + bad + "\"\n")); err == nil {
				t.Fatalf("listen %q should be rejected", bad)
			}
		})
	}
}

func TestResolveEnv(t *testing.T) {
	t.Setenv("LGLLM_TEST_KEY", "sk-from-env")
	cfg, err := Parse([]byte(`
server:
  listen: 127.0.0.1:20128
auth:
  mode: required
  keys:
    - ${LGLLM_TEST_KEY}
    - ${LGLLM_TEST_MISSING:-sk-default-value}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Auth.Keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(cfg.Auth.Keys))
	}
	if cfg.Auth.Keys[0] != "sk-from-env" {
		t.Errorf("key[0] = %q", cfg.Auth.Keys[0])
	}
	if cfg.Auth.Keys[1] != "sk-default-value" {
		t.Errorf("key[1] = %q", cfg.Auth.Keys[1])
	}
}

func TestResolveEnvMissingIsFatal(t *testing.T) {
	_, err := Parse([]byte(`
server:
  listen: 127.0.0.1:20128
auth:
  keys: ["${LGLLM_DEFINITELY_NOT_SET}"]
`))
	if err == nil || !strings.Contains(err.Error(), "LGLLM_DEFINITELY_NOT_SET") {
		t.Fatalf("missing variable must fail, got %v", err)
	}
}

func TestResolveEnvDoesNotTouchMappingKeys(t *testing.T) {
	t.Setenv("LGLLM_TEST_VAL", "v")
	data := []byte("server:\n  listen: 127.0.0.1:20128\nlog:\n  level: ${LGLLM_TEST_VAL}\n")
	if _, err := Parse(data); err == nil {
		t.Fatal("level ${LGLLM_TEST_VAL}=v should fail validation, proving expansion happened")
	}
}

func TestFingerprintChanges(t *testing.T) {
	a, err := Parse([]byte("server:\n  listen: 127.0.0.1:20128\nlog:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Parse([]byte("server:\n  listen: 127.0.0.1:20128\nlog:\n  level: info\n"))
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("identical configs must fingerprint identically")
	}
	c, _ := Parse([]byte("server:\n  listen: 127.0.0.1:20128\nlog:\n  level: debug\n"))
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatal("a changed level must change the fingerprint")
	}
}

func TestRedactedHidesKeys(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  listen: 127.0.0.1:20128
auth:
  keys: ["sk-supersecretvalue"]
`))
	if err != nil {
		t.Fatal(err)
	}
	out := cfg.Redacted()
	if strings.Contains(out, "supersecretvalue") {
		t.Fatalf("redacted config leaks the key:\n%s", out)
	}
}

func TestLoadSetsSourceAndDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen: 127.0.0.1:20128\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	abs, _ := filepath.Abs(path)
	if cfg.Source != abs {
		t.Errorf("Source = %q, want %q", cfg.Source, abs)
	}
	if cfg.Dir != filepath.Dir(abs) {
		t.Errorf("Dir = %q", cfg.Dir)
	}
}
