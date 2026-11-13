package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lgllm/internal/registry"
)

func sessionPayload(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestConversationSessionFingerprintIsStableAcrossTurns(t *testing.T) {
	first := sessionPayload(t, `{
		"messages":[
			{"role":"user","content":"fix the failing test"},
			{"role":"assistant","content":"working on it"}
		]
	}`)
	second := sessionPayload(t, `{
		"messages":[
			{"role":"user","content":"fix the failing test"},
			{"role":"assistant","content":"working on it"},
			{"role":"user","content":"thanks, now run it again"}
		]
	}`)
	other := sessionPayload(t, `{"messages":[{"role":"user","content":"write a different feature"}]}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	a := conversationSession(req, first, "req-a", "key:one")
	b := conversationSession(req, second, "req-b", "key:one")
	c := conversationSession(req, other, "req-c", "key:one")

	if a == "" || b == "" {
		t.Fatal("a request with a user message must get a session id")
	}
	if a != b {
		t.Errorf("same conversation moved from %q to %q", a, b)
	}
	if a == c {
		t.Errorf("different conversations share %q", a)
	}
}

func TestConversationSessionPrefersClientHeader(t *testing.T) {
	payload := sessionPayload(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Lgllm-Session", "explicit-session")
	if got := conversationSession(req, payload, "req", "key"); got != "explicit-session" {
		t.Errorf("session = %q, want the explicit header", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("x-opencode-session", "opencode-session")
	if got := conversationSession(req, payload, "req", "key"); got != "opencode-session" {
		t.Errorf("session = %q, want x-opencode-session", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("x-session-affinity", "affinity-session")
	if got := conversationSession(req, payload, "req", "key"); got != "affinity-session" {
		t.Errorf("session = %q, want the native header value", got)
	}
}

func TestConversationSessionReadsDialects(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "anthropic system and content parts",
			body: `{"system":[{"type":"text","text":"be terse"}],"messages":[{"role":"user","content":[{"type":"text","text":"anthropic prompt"}]}]}`,
		},
		{
			name: "responses input string",
			body: `{"instructions":"be terse","input":"responses prompt"}`,
		},
		{
			name: "legacy prompt",
			body: `{"prompt":"legacy prompt"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			payload := sessionPayload(t, tc.body)
			if got := conversationSession(req, payload, "req", "key"); got == "" {
				t.Fatal("no session id was derived")
			}
		})
	}
}

func TestSessionKeyIgnoresBodyFingerprint(t *testing.T) {
	payload := sessionPayload(t, `{"messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := sessionKey(req, payload); got != "" {
		t.Errorf("sessionKey = %q, want empty so round-robin still rotates", got)
	}
}

func TestApplySessionHeaderOnlyForCapableProvider(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	applySessionHeader(req, &registry.Provider{SessionHeader: "x-opencode-session"}, "ses_abc")
	if got := req.Header.Get("x-opencode-session"); got != "ses_abc" {
		t.Errorf("header = %q", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	applySessionHeader(req, &registry.Provider{}, "ses_abc")
	if got := req.Header.Get("x-opencode-session"); got != "" {
		t.Errorf("a provider without the capability got %q", got)
	}
}
