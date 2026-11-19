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
	if !strings.Contains(js.Body.String(), "/api/performance?series=1") {
		t.Error("app.js does not request the performance time series")
	}
	if !strings.Contains(js.Body.String(), "low_sample") || !strings.Contains(js.Body.String(), "connections") {
		t.Error("app.js does not render the low-sample and per-connection fields")
	}
	if !strings.Contains(js.Body.String(), "percentileOrAvg") || !strings.Contains(js.Body.String(), "p50_") {
		t.Error("app.js does not use the p50 performance fields")
	}
	if !strings.Contains(js.Body.String(), "max_concurrent") {
		t.Error("app.js does not expose max_concurrent in the connection form")
	}
	if !strings.Contains(js.Body.String(), "lgllm.perf.scale") || !strings.Contains(js.Body.String(), "logTicks") {
		t.Error("app.js does not implement the performance log-scale toggle")
	}
	if !strings.Contains(js.Body.String(), "i/o/c") || !strings.Contains(js.Body.String(), "ratesCell") {
		t.Error("app.js does not render the pricing column")
	}
	if !strings.Contains(js.Body.String(), "capabilityIcons") {
		t.Error("app.js does not render model capability icons")
	}
	if !strings.Contains(js.Body.String(), "/api/providers?all=1") ||
		!strings.Contains(js.Body.String(), "/api/catalog") {
		t.Error("app.js does not load the provider catalog")
	}
	if !strings.Contains(js.Body.String(), "/api/keys/generate") ||
		!strings.Contains(js.Body.String(), "Generate key") {
		t.Error("app.js does not expose key generation")
	}
	if !strings.Contains(js.Body.String(), "Rename") ||
		!strings.Contains(js.Body.String(), "openGenerateKeyForm") {
		t.Error("app.js does not expose key names and rename")
	}
	if !strings.Contains(js.Body.String(), "ctx min") || !strings.Contains(js.Body.String(), "fmtContext") {
		t.Error("app.js does not show the combo context window")
	}
	if !strings.Contains(page.Body.String(), `data-tab="performance"`) {
		t.Error("index does not have a performance tab")
	}
	for _, want := range []string{`rel="icon"`, `apple-touch-icon`, `manifest.json`, `brand-icon`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("index does not reference %q", want)
		}
	}

	icon := do(s, httptest.NewRequest(http.MethodGet, "/ui/favicon-32.png", nil))
	if icon.Code != http.StatusOK {
		t.Fatalf("favicon status = %d", icon.Code)
	}
	if ct := icon.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("favicon Content-Type = %q", ct)
	}
	if len(icon.Body.Bytes()) < 100 {
		t.Errorf("favicon is unexpectedly small: %d bytes", icon.Body.Len())
	}
	manifest := do(s, httptest.NewRequest(http.MethodGet, "/ui/manifest.json", nil))
	if manifest.Code != http.StatusOK {
		t.Fatalf("manifest status = %d", manifest.Code)
	}
	if !strings.Contains(manifest.Body.String(), `"short_name": "lgllm"`) {
		t.Errorf("manifest body = %s", manifest.Body)
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
