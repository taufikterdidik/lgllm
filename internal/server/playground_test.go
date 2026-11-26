package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAdminPlaygroundModelsAndChat(t *testing.T) {
	s, _, _ := newAdminTestServer(t, true)

	if res := do(s, adminRequest(http.MethodGet, "/api/playground/models", "", "")); res.Code != http.StatusUnauthorized {
		t.Errorf("models without admin token = %d, want 401", res.Code)
	}
	models := do(s, adminRequest(http.MethodGet, "/api/playground/models", adminTestToken, ""))
	if models.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", models.Code, models.Body)
	}
	if !strings.Contains(models.Body.String(), "testprov/test-model") {
		t.Errorf("models body = %s", models.Body)
	}

	chat := do(s, adminRequest(http.MethodPost, "/api/playground/chat", adminTestToken,
		`{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`))
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", chat.Code, chat.Body)
	}
	if !strings.Contains(chat.Body.String(), `"content":"ok"`) {
		t.Errorf("chat body = %s", chat.Body)
	}

	bad := do(s, adminRequest(http.MethodPost, "/api/playground/chat", adminTestToken,
		`{"messages":[{"role":"user","content":"hi"}]}`))
	if bad.Code != http.StatusBadRequest {
		t.Errorf("missing model status = %d, body = %s", bad.Code, bad.Body)
	}
}

func TestAdminPlaygroundStreamsSSE(t *testing.T) {
	events := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	s, _, _ := newAdminTestServerWith(t, false, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, event := range events {
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	res := do(s, adminRequest(http.MethodPost, "/api/playground/chat", adminTestToken,
		`{"model":"testprov/test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if ct := res.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if got, want := res.Body.String(), strings.Join(events, ""); got != want {
		t.Errorf("stream = %q, want %q", got, want)
	}
}
