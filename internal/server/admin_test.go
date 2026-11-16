package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"lgllm/internal/admin"
	"lgllm/internal/auth"
	"lgllm/internal/config"
	"lgllm/internal/metrics"
	"lgllm/internal/registry"
	"lgllm/internal/upstream"
	"lgllm/internal/usagelog"
)

const adminTestToken = "sk-admin-token-123"

// newAdminTestServer writes the standard test config to a temp file, builds an
// admin manager over it, and wires the manager's Apply into the registry the
// server reads.
func newAdminTestServer(t *testing.T, usage bool) (*Server, *admin.Manager, string) {
	t.Helper()
	return newAdminTestServerWith(t, usage, nil, nil)
}

// newAdminTestServerWith is newAdminTestServer with a config hook and an
// upstream handler, so a test can observe what the admin API puts on the wire.
func newAdminTestServerWith(t *testing.T, usage bool, mutate func(*config.Config), handler http.HandlerFunc) (*Server, *admin.Manager, string) {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
			  "choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			  "usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}
			}`)
		}
	}
	_, cfg := mockUpstream(t, handler)
	if mutate != nil {
		mutate(cfg)
	}
	cfg.Admin = config.Admin{Enabled: true, Token: adminTestToken}
	cfg.TokenSaver.Enabled = false

	dir := t.TempDir()
	base := filepath.Join(dir, "config.yaml")
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var regPtr atomic.Pointer[registry.Registry]
	regPtr.Store(mustRegistry(t, cfg))
	apply := func(c *config.Config) error {
		next, err := registry.New(c)
		if err != nil {
			return err
		}
		regPtr.Store(next)
		return nil
	}
	mgr, err := admin.New(admin.Options{
		BasePath:  base,
		StatePath: filepath.Join(dir, "admin.yaml"),
		Apply:     apply,
	})
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var store *usagelog.Store
	if usage {
		store = usagelog.New(100)
	}
	srv := New(Options{
		Config:   mgr.Config,
		Log:      logger,
		Auth:     auth.New(nil, true),
		Registry: regPtr.Load,
		Upstream: upstream.New(cfg.Upstream),
		Metrics:  metrics.New(),
		Usage:    store,
		Admin:    mgr,
	})
	return srv, mgr, dir
}

func adminRequest(method, path, token, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	return req
}

func TestAdminDisabledReturns404(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) { c.Admin.Enabled = false })
	s := newTestServer(t, cfg, nil)
	res := do(s, adminRequest(http.MethodGet, "/api/providers", "", ""))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
}

func TestAdminAuthIsSeparate(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)

	if res := do(s, adminRequest(http.MethodGet, "/api/providers", "", "")); res.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d", res.Code)
	}
	req := adminRequest(http.MethodGet, "/api/providers", "", "")
	req.Header.Set("Authorization", "Bearer sk-upstream") // an inference key
	if res := do(s, req); res.Code != http.StatusUnauthorized {
		t.Errorf("inference key accepted: status = %d", res.Code)
	}
	if res := do(s, adminRequest(http.MethodGet, "/api/providers", adminTestToken, "")); res.Code != http.StatusOK {
		t.Errorf("admin token rejected: status = %d, body = %s", res.Code, res.Body)
	}
}

func TestAdminCatalogListsBuiltinProviders(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	res := do(s, adminRequest(http.MethodGet, "/api/catalog", adminTestToken, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var body struct {
		Object string                 `json:"object"`
		Data   []adminCatalogProvider `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "list" || len(body.Data) < 5 {
		t.Fatalf("catalog = %+v", body)
	}
	byID := make(map[string]adminCatalogProvider, len(body.Data))
	for _, p := range body.Data {
		byID[p.ID] = p
	}
	openai, ok := byID["openai"]
	if !ok {
		t.Fatal("openai missing from catalog")
	}
	if openai.BaseURL == "" || openai.Format == "" || openai.KeyEnv == "" || len(openai.Models) == 0 {
		t.Errorf("openai catalog entry = %+v", openai)
	}
	if _, ok := byID["commandcode"]; !ok {
		t.Error("commandcode missing from catalog")
	}
}

func TestAdminProvidersAllIncludesCatalogAndConnectionState(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	res := do(s, adminRequest(http.MethodGet, "/api/providers?all=1", adminTestToken, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var body struct {
		Data []adminProvider `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]adminProvider, len(body.Data))
	for _, p := range body.Data {
		byID[p.ID] = p
	}
	testprov, ok := byID["testprov"]
	if !ok {
		t.Fatal("configured provider missing")
	}
	if !testprov.Connected || testprov.Connections != 1 {
		t.Errorf("testprov connection state = connected:%v count:%d", testprov.Connected, testprov.Connections)
	}
	openai, ok := byID["openai"]
	if !ok {
		t.Fatal("catalog provider missing with ?all=1")
	}
	if openai.Managed != "catalog" || openai.Connected || openai.Connections != 0 {
		t.Errorf("openai = %+v", openai)
	}
	if openai.KeyEnv == "" || openai.BaseURL == "" {
		t.Errorf("openai metadata incomplete: %+v", openai)
	}
}

func TestAdminGenerateKey(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	res := do(s, adminRequest(http.MethodPost, "/api/keys/generate", adminTestToken, `{"name":"generated-key"}`))
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var created adminKey
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, "sk-lgllm-") || len(created.Key) < 32 {
		t.Errorf("generated key = %q", created.Key)
	}
	if created.ID == "" || created.Source != "admin" || created.Name != "generated-key" {
		t.Errorf("generated metadata = %+v", created)
	}

	list := do(s, adminRequest(http.MethodGet, "/api/keys", adminTestToken, ""))
	if strings.Contains(list.Body.String(), created.Key) {
		t.Error("GET /api/keys leaked the generated plaintext key")
	}
	if !strings.Contains(list.Body.String(), created.ID) || !strings.Contains(list.Body.String(), `"name":"generated-key"`) {
		t.Errorf("generated key id/name missing from list: %s", list.Body)
	}
}

func TestAdminProviderCrud(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)

	list := do(s, adminRequest(http.MethodGet, "/api/providers", adminTestToken, ""))
	if !strings.Contains(list.Body.String(), `"managed":"config"`) {
		t.Errorf("config provider not listed as managed: %s", list.Body)
	}

	create := do(s, adminRequest(http.MethodPost, "/api/providers", adminTestToken,
		`{"id":"extra","base_url":"https://extra.example/v1","format":"openai","auth":"bearer","models":[{"id":"m"}]}`))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", create.Code, create.Body)
	}
	list = do(s, adminRequest(http.MethodGet, "/api/providers", adminTestToken, ""))
	if !strings.Contains(list.Body.String(), `"id":"extra"`) || !strings.Contains(list.Body.String(), `"managed":"admin"`) {
		t.Errorf("admin provider not listed: %s", list.Body)
	}

	update := do(s, adminRequest(http.MethodPut, "/api/providers/extra", adminTestToken,
		`{"base_url":"https://changed.example/v1","format":"openai","auth":"bearer","models":[{"id":"m2"}]}`))
	if update.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", update.Code, update.Body)
	}

	if del := do(s, adminRequest(http.MethodDelete, "/api/providers/extra", adminTestToken, "")); del.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", del.Code, del.Body)
	}

	// config-managed resources are read-only.
	ro := do(s, adminRequest(http.MethodDelete, "/api/providers/testprov", adminTestToken, ""))
	if ro.Code != http.StatusConflict {
		t.Errorf("config provider delete status = %d", ro.Code)
	}
}

func TestAdminConnectionCrudRedactsKey(t *testing.T) {
	s, mgr, _ := newAdminTestServer(t, false)
	do(s, adminRequest(http.MethodPost, "/api/providers", adminTestToken,
		`{"id":"p2","base_url":"https://p2.example/v1","format":"openai","auth":"bearer","models":[{"id":"m"}]}`))

	create := do(s, adminRequest(http.MethodPost, "/api/connections", adminTestToken,
		`{"id":"c2","provider":"p2","api_key":"sk-secret-value"}`))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", create.Code, create.Body)
	}
	list := do(s, adminRequest(http.MethodGet, "/api/connections", adminTestToken, ""))
	if strings.Contains(list.Body.String(), "sk-secret-value") {
		t.Error("the API leaked a connection key")
	}
	if !strings.Contains(list.Body.String(), `"api_key":"***"`) {
		t.Errorf("key not redacted: %s", list.Body)
	}

	// Sending "***" back keeps the stored key.
	update := do(s, adminRequest(http.MethodPut, "/api/connections/c2", adminTestToken,
		`{"provider":"p2","api_key":"***","base_url":"https://c2.example"}`))
	if update.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", update.Code, update.Body)
	}
	for _, c := range mgr.State().Connections {
		if c.ID == "c2" && c.APIKey != "sk-secret-value" {
			t.Errorf("stored key = %q", c.APIKey)
		}
	}

	if del := do(s, adminRequest(http.MethodDelete, "/api/connections/c2", adminTestToken, "")); del.Code != http.StatusNoContent {
		t.Errorf("delete status = %d", del.Code)
	}
	do(s, adminRequest(http.MethodDelete, "/api/providers/p2", adminTestToken, ""))
}

func TestAdminComboAndKeyCrud(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)

	create := do(s, adminRequest(http.MethodPost, "/api/combos", adminTestToken,
		`{"name":"extra-combo","members":["testprov/test-model"]}`))
	if create.Code != http.StatusCreated {
		t.Fatalf("combo create status = %d, body = %s", create.Code, create.Body)
	}
	if put := do(s, adminRequest(http.MethodPut, "/api/combos/extra-combo", adminTestToken,
		`{"members":["testprov/test-model"],"strategy":"priority"}`)); put.Code != http.StatusOK {
		t.Errorf("combo update status = %d, body = %s", put.Code, put.Body)
	}
	if del := do(s, adminRequest(http.MethodDelete, "/api/combos/extra-combo", adminTestToken, "")); del.Code != http.StatusNoContent {
		t.Errorf("combo delete status = %d", del.Code)
	}

	key := do(s, adminRequest(http.MethodPost, "/api/keys", adminTestToken, `{"key":"sk-admin-created-1","name":"laptop"}`))
	if key.Code != http.StatusCreated {
		t.Fatalf("key create status = %d, body = %s", key.Code, key.Body)
	}
	var created adminKey
	if err := json.Unmarshal(key.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	list := do(s, adminRequest(http.MethodGet, "/api/keys", adminTestToken, ""))
	if !strings.Contains(list.Body.String(), created.ID) || !strings.Contains(list.Body.String(), `"name":"laptop"`) {
		t.Errorf("key id/name missing from list: %s", list.Body)
	}
	rename := do(s, adminRequest(http.MethodPut, "/api/keys/"+created.ID, adminTestToken, `{"name":"workstation"}`))
	if rename.Code != http.StatusOK {
		t.Fatalf("key rename status = %d, body = %s", rename.Code, rename.Body)
	}
	list = do(s, adminRequest(http.MethodGet, "/api/keys", adminTestToken, ""))
	if !strings.Contains(list.Body.String(), `"name":"workstation"`) {
		t.Errorf("renamed key missing from list: %s", list.Body)
	}
	if del := do(s, adminRequest(http.MethodDelete, "/api/keys/"+created.ID, adminTestToken, "")); del.Code != http.StatusNoContent {
		t.Errorf("key delete status = %d, body = %s", del.Code, del.Body)
	}
}

func TestAdminUsageAndTestConnection(t *testing.T) {
	s, _, _ := newAdminTestServer(t, true)

	chat := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`)))
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", chat.Code, chat.Body)
	}

	usage := do(s, adminRequest(http.MethodGet, "/api/usage?group=provider", adminTestToken, ""))
	if usage.Code != http.StatusOK {
		t.Fatalf("usage status = %d, body = %s", usage.Code, usage.Body)
	}
	var payload struct {
		Group string `json:"group"`
		Data  []struct {
			Key              string `json:"key"`
			Requests         int    `json:"requests"`
			PromptTokens     int    `json:"prompt_tokens"`
			CompletionTokens int    `json:"completion_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(usage.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body = %s: %v", usage.Body, err)
	}
	if payload.Group != "provider" || len(payload.Data) != 1 {
		t.Fatalf("usage = %+v", payload)
	}
	if payload.Data[0].Key != "testprov" || payload.Data[0].Requests != 1 {
		t.Errorf("bucket = %+v", payload.Data[0])
	}
	if payload.Data[0].PromptTokens != 7 || payload.Data[0].CompletionTokens != 3 {
		t.Errorf("tokens = %+v", payload.Data[0])
	}

	test := do(s, adminRequest(http.MethodPost, "/api/test-connection", adminTestToken, `{"provider":"testprov"}`))
	if test.Code != http.StatusOK {
		t.Fatalf("test-connection status = %d, body = %s", test.Code, test.Body)
	}
	var result struct {
		OK        bool `json:"ok"`
		Reachable bool `json:"reachable"`
		Status    int  `json:"status"`
	}
	if err := json.Unmarshal(test.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.Reachable || result.Status != 200 {
		t.Errorf("test-connection = %+v", result)
	}
}

func TestAdminTestConnectionSendsSessionHeader(t *testing.T) {
	var got string
	s, _, _ := newAdminTestServerWith(t, false, func(c *config.Config) {
		c.Providers[0].SessionHeader = "x-opencode-session"
	}, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Opencode-Session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})

	res := do(s, adminRequest(http.MethodPost, "/api/test-connection", adminTestToken,
		`{"provider":"testprov","session":"ses_admin_test"}`))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if got != "ses_admin_test" {
		t.Errorf("upstream session = %q, want the explicit value", got)
	}

	// Without an explicit id the admin probe still needs a non-empty stable id
	// or a gateway with a mandatory session header rejects it.
	got = ""
	res = do(s, adminRequest(http.MethodPost, "/api/test-connection", adminTestToken,
		`{"provider":"testprov"}`))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if got == "" {
		t.Error("test-connection sent no session header")
	}
}

func TestAdminLogsRequiresToken(t *testing.T) {
	s, _, _ := newAdminTestServer(t, true)
	if res := do(s, adminRequest(http.MethodGet, "/api/logs", adminTestToken, "")); res.Code != http.StatusOK {
		t.Errorf("logs status = %d, body = %s", res.Code, res.Body)
	}
	if res := do(s, adminRequest(http.MethodGet, "/api/usage", adminTestToken, "")); res.Code != http.StatusOK {
		t.Errorf("usage status = %d", res.Code)
	}
}

func TestAdminLogsStream(t *testing.T) {
	s, _, _ := newAdminTestServer(t, true)
	front := httptest.NewServer(s.Handler())
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/api/logs/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Admin-Token", adminTestToken)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", stream.StatusCode)
	}

	// The handler subscribes before flushing the headers, so this request is
	// guaranteed to be observed.
	chat, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, chat.Body)
	_ = chat.Body.Close()

	scanner := bufio.NewScanner(stream.Body)
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var entry struct {
			Status   int    `json:"status"`
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &entry); err != nil {
			t.Fatalf("event = %s: %v", line, err)
		}
		if entry.Status == 200 && entry.Provider == "testprov" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the stream did not deliver the request entry")
	}
}
