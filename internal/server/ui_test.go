package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lgllm/internal/config"
)

func TestAdminUIIsEmbeddedAndGated(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)

	page := do(s, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("ui status = %d, body = %s", page.Code, page.Body)
	}
	if !strings.Contains(page.Body.String(), "lgllm admin") {
		t.Errorf("index does not look like the UI: %s", page.Body)
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}

	js := do(s, httptest.NewRequest(http.MethodGet, "/ui/app.js", nil))
	if js.Code != http.StatusOK {
		t.Fatalf("app.js status = %d", js.Code)
	}
	if ct := js.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q", ct)
	}
	if !strings.Contains(js.Body.String(), "/api/providers") {
		t.Error("app.js does not call the admin API")
	}
	if !strings.Contains(js.Body.String(), "/api/performance") {
		t.Error("app.js does not call the performance endpoint")
	}
	if !strings.Contains(page.Body.String(), `data-tab="performance"`) {
		t.Error("index does not have a performance tab")
	}

	// /ui redirects to /ui/.
	redirect := do(s, httptest.NewRequest(http.MethodGet, "/ui", nil))
	if redirect.Code != http.StatusMovedPermanently || redirect.Header().Get("Location") != "/ui/" {
		t.Errorf("redirect = %d %q", redirect.Code, redirect.Header().Get("Location"))
	}
}

func TestAdminUIDisabled(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) { c.Admin.Enabled = false })
	s := newTestServer(t, cfg, nil)
	res := do(s, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
}

func TestAdminUIIsPublicWhileAPIIsNot(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)

	// The static page does not consume an inference key...
	if res := do(s, httptest.NewRequest(http.MethodGet, "/ui/", nil)); res.Code != http.StatusOK {
		t.Errorf("ui status = %d", res.Code)
	}
	// ...but the admin API still requires its own token.
	if res := do(s, httptest.NewRequest(http.MethodGet, "/api/providers", nil)); res.Code != http.StatusUnauthorized {
		t.Errorf("admin API status = %d, want 401", res.Code)
	}
}
