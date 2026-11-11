package configcmd

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lgllm/internal/config"
)

func runCLI(t *testing.T, path, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	full := append([]string{"--config", path}, args...)
	err := Run(full, strings.NewReader(stdin), &out, &out)
	return out.String(), err
}

func mustRun(t *testing.T, path, stdin string, args ...string) string {
	t.Helper()
	out, err := runCLI(t, path, stdin, args...)
	if err != nil {
		t.Fatalf("%v failed: %v\n%s", args, err, out)
	}
	return out
}

func readConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}

func TestInitValidateShow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustRun(t, path, "", "init")

	out := mustRun(t, path, "", "validate")
	if !strings.Contains(out, "config OK") {
		t.Fatalf("validate output: %s", out)
	}
	out = mustRun(t, path, "", "show")
	if !strings.Contains(out, "auth.mode: localhost") {
		t.Fatalf("show output: %s", out)
	}

	if _, err := runCLI(t, path, "", "init"); err == nil {
		t.Fatal("init over an existing file should fail without --force")
	}
	mustRun(t, path, "", "init", "--force")
}

func TestProviderAndConnectionLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustRun(t, path, "", "init")
	mustRun(t, path, "", "provider", "add", "--id", "local",
		"--base-url", "https://example.test/v1", "--format", "openai", "--auth", "bearer",
		"--model", "alpha,beta")

	cfg := readConfig(t, path)
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "local" {
		t.Fatalf("providers: %+v", cfg.Providers)
	}
	if len(cfg.Providers[0].Models) != 2 {
		t.Fatalf("models: %+v", cfg.Providers[0].Models)
	}

	if _, err := runCLI(t, path, "", "provider", "add", "local"); err == nil {
		t.Fatal("duplicate provider add should fail")
	}

	mustRun(t, path, "", "connection", "add", "--provider", "local", "--id", "main", "--api-key", "secret")
	cfg = readConfig(t, path)
	if len(cfg.Connections) != 1 || cfg.Connections[0].APIKey != "secret" {
		t.Fatalf("connections: %+v", cfg.Connections)
	}

	if _, err := runCLI(t, path, "", "connection", "add", "--provider", "local", "--id", "main", "--api-key", "x"); err == nil {
		t.Fatal("duplicate connection add should fail")
	}
	if _, err := runCLI(t, path, "", "provider", "remove", "local"); err == nil {
		t.Fatal("provider remove with a live connection should fail")
	}

	mustRun(t, path, "", "connection", "set", "--id", "main", "--api-key", "rotated")
	if got := readConfig(t, path).Connections[0].APIKey; got != "rotated" {
		t.Fatalf("api key = %q", got)
	}

	mustRun(t, path, "", "connection", "remove", "main")
	mustRun(t, path, "", "provider", "remove", "local")
	cfg = readConfig(t, path)
	if len(cfg.Providers) != 0 || len(cfg.Connections) != 0 {
		t.Fatalf("config not empty: %+v", cfg)
	}
}

func TestCatalogAliasAndEnvPlaceholder(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustRun(t, path, "", "init")
	// A catalog provider needs no `provider add`; the connection alone activates it.
	mustRun(t, path, "", "connection", "add", "--provider", "oa", "--id", "work")
	mustRun(t, path, "", "provider", "add", "oa")

	cfg := readConfig(t, path)
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "openai" {
		t.Fatalf("catalog alias was not canonicalized: %+v", cfg.Providers)
	}
	if got := cfg.Connections[0].Provider; got != "openai" {
		t.Fatalf("connection provider = %q", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "${OPENAI_API_KEY}") {
		t.Fatalf("file should keep the environment placeholder:\n%s", raw)
	}
	if got := cfg.Connections[0].APIKey; got != "sk-test" {
		t.Fatalf("resolved api key = %q", got)
	}
	out := mustRun(t, path, "", "show")
	if strings.Contains(out, "sk-test") {
		t.Fatalf("show leaked the resolved key: %s", out)
	}
	if !strings.Contains(out, "${OPENAI_API_KEY}") {
		t.Fatalf("show should display the placeholder: %s", out)
	}
}

const syncConfig = `server:
  listen: 127.0.0.1:0
providers:
  - id: local
    base_url: %s/v1
    format: openai
    auth: bearer
    models:
      - id: alpha-1
        name: Alpha One
        context: 8192
        capabilities:
          vision: true
      - id: old-1
connections:
  - id: main
    provider: local
    api_key: secret
`

func TestModelsSyncMergeAndReplace(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"alpha-1"},{"id":"beta-2"}]}`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(syncConfig, srv.URL)), 0o600); err != nil {
		t.Fatal(err)
	}

	out := mustRun(t, path, "", "models", "sync", "--provider", "local")
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if !strings.Contains(out, "1 new, 2 kept, 0 removed") {
		t.Fatalf("unexpected summary: %s", out)
	}
	cfg := readConfig(t, path)
	models := cfg.Providers[0].Models
	if len(models) != 3 || models[0].ID != "alpha-1" || models[1].ID != "old-1" || models[2].ID != "beta-2" {
		t.Fatalf("merged models: %+v", models)
	}
	if models[0].Name != "Alpha One" || models[0].Context != 8192 || !models[0].Capabilities["vision"] {
		t.Fatalf("metadata was not preserved: %+v", models[0])
	}

	// Dry-run must not touch the file.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out = mustRun(t, path, "", "models", "sync", "--provider", "local", "--replace", "--dry-run")
	if !strings.Contains(out, "would sync") {
		t.Fatalf("dry-run output: %s", out)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dry-run modified the file")
	}

	mustRun(t, path, "", "models", "sync", "--provider", "local", "--replace")
	models = readConfig(t, path).Providers[0].Models
	if len(models) != 2 || models[0].ID != "alpha-1" || models[1].ID != "beta-2" {
		t.Fatalf("replaced models: %+v", models)
	}
	if models[0].Name != "Alpha One" {
		t.Fatalf("metadata lost on replace: %+v", models[0])
	}
}

func TestModelsSyncAuthSchemes(t *testing.T) {
	tests := []struct {
		name  string
		auth  string
		check func(*http.Request) bool
	}{
		{"bearer", "bearer", func(r *http.Request) bool {
			return r.Header.Get("Authorization") == "Bearer secret"
		}},
		{"x-api-key", "x-api-key", func(r *http.Request) bool {
			return r.Header.Get("x-api-key") == "secret"
		}},
		{"query", "query", func(r *http.Request) bool {
			return r.URL.Query().Get("key") == "secret"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !tc.check(r) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				fmt.Fprint(w, `{"models":[{"name":"ollama-model"}]}`)
			})
			srv := httptest.NewServer(handler)
			defer srv.Close()

			extra := ""
			if tc.auth == "query" {
				extra = "    auth_param: key\n"
			}
			body := fmt.Sprintf(`server:
  listen: 127.0.0.1:0
providers:
  - id: local
    base_url: %s/v1
    format: openai
    auth: %s
%sconnections:
  - id: main
    provider: local
    api_key: secret
`, srv.URL, tc.auth, extra)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			mustRun(t, path, "", "models", "sync", "--provider", "local")
			models := readConfig(t, path).Providers[0].Models
			if len(models) != 1 || models[0].ID != "ollama-model" {
				t.Fatalf("models: %+v", models)
			}
		})
	}
}

func TestModelsSyncFailureLeavesFileAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf(syncConfig, srv.URL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, path, "", "models", "sync", "--provider", "local"); err == nil {
		t.Fatal("sync against a 401 should fail")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed sync modified the file")
	}
}

func TestModelsSyncRefusesAdminManagedProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"alpha-1"}]}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	main := `server:
  listen: 127.0.0.1:0
connections:
  - id: main
    provider: local
    api_key: secret
`
	if err := os.WriteFile(path, []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := fmt.Sprintf(`providers:
  - id: local
    base_url: %s/v1
    format: openai
    auth: bearer
`, srv.URL)
	if err := os.WriteFile(filepath.Join(dir, "admin.yaml"), []byte(admin), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runCLI(t, path, "", "models", "sync", "--provider", "local")
	if err == nil || !strings.Contains(err.Error(), "admin state file") {
		t.Fatalf("want admin-state error, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused sync modified the main file")
	}
}

func TestConnectionTest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"alpha-1"},{"id":"beta-2"}]}`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(syncConfig, srv.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, path, "", "connection", "test", "--provider", "local")
	if !strings.Contains(out, "returned 2 model(s)") || !strings.Contains(out, "beta-2") {
		t.Fatalf("connection test output: %s", out)
	}
}

func TestAliasAndCombo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustRun(t, path, "", "init")
	mustRun(t, path, "", "alias", "set", "fast", "ds/deepseek-chat")
	out := mustRun(t, path, "", "alias", "list")
	if !strings.Contains(out, "fast") || !strings.Contains(out, "ds/deepseek-chat") {
		t.Fatalf("alias list: %s", out)
	}

	mustRun(t, path, "", "combo", "add", "cheap",
		"--member", "ds/deepseek-chat", "--member", "or/google/gemini-2.5-pro",
		"--strategy", "round-robin", "--token-saver", "false")
	cfg := readConfig(t, path)
	if len(cfg.Combos) != 1 {
		t.Fatalf("combos: %+v", cfg.Combos)
	}
	cb := cfg.Combos[0]
	if cb.Name != "cheap" || len(cb.Members) != 2 || cb.Strategy != "round-robin" || cb.TokenSaver == nil || *cb.TokenSaver {
		t.Fatalf("combo: %+v", cb)
	}
	if _, err := runCLI(t, path, "", "combo", "add", "cheap", "--member", "ds/deepseek-chat"); err == nil {
		t.Fatal("duplicate combo add should fail")
	}
	mustRun(t, path, "", "combo", "set", "cheap", "--member", "ds/deepseek-chat")
	if got := len(readConfig(t, path).Combos[0].Members); got != 1 {
		t.Fatalf("members after set = %d", got)
	}

	mustRun(t, path, "", "combo", "remove", "cheap")
	mustRun(t, path, "", "alias", "remove", "fast")
	cfg = readConfig(t, path)
	if len(cfg.Combos) != 0 || len(cfg.Aliases) != 0 {
		t.Fatalf("shortcuts not removed: %+v %+v", cfg.Combos, cfg.Aliases)
	}
}

func TestWizardCreatesProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	// Create the starter file, add the openai catalog provider, decline the
	// connection prompt, then quit.
	script := "y\n1\nopenai\nn\n9\n"
	out, err := runCLI(t, path, script, "wizard")
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	cfg := readConfig(t, path)
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "openai" {
		t.Fatalf("providers: %+v", cfg.Providers)
	}
}

func TestGlobalFlagAfterCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out bytes.Buffer
	if err := Run([]string{"init", "--config", path}, strings.NewReader(""), &out, &out); err != nil {
		t.Fatalf("init with trailing --config: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not created: %v", err)
	}
}

func TestNonInteractiveNoCommandPrintsHelp(t *testing.T) {
	out, err := runCLI(t, "unused.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Usage: lgllm config") {
		t.Fatalf("expected usage, got: %s", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	if _, err := runCLI(t, "unused.yaml", "", "nope"); err == nil {
		t.Fatal("unknown command should fail")
	}
}
