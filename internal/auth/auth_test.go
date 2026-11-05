package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestVerifyAcceptsConfiguredKeys(t *testing.T) {
	v := New([]string{"sk-raw-secret", digest("sk-hashed-secret")}, false)

	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{"raw key", "sk-raw-secret", true},
		{"hashed key", "sk-hashed-secret", true},
		{"wrong key", "sk-nope", false},
		{"prefix of a key", "sk-raw-secre", false},
		{"empty token", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := v.Verify(tt.token); got != tt.want {
				t.Fatalf("Verify(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestMalformedDigestIsNotAKey(t *testing.T) {
	v := New([]string{"sha256:zzzz", "sha256:" + hex.EncodeToString([]byte("short"))}, false)
	if !v.Empty() {
		t.Fatal("malformed digests must not become usable keys")
	}
}

func TestDisabledAndEmpty(t *testing.T) {
	if New(nil, false).Disabled() {
		t.Error("a verifier without keys is not disabled; it fails closed")
	}
	if !New(nil, false).Empty() {
		t.Error("no keys should report empty")
	}
	if New([]string{"k"}, false).Disabled() {
		t.Error("explicitly enabled verifier must not report disabled")
	}
	if !New([]string{"k"}, true).Disabled() {
		t.Error("explicit disabled should win")
	}
	v := New([]string{"k"}, false)
	if v.Disabled() || v.Empty() {
		t.Error("configured key should be enabled and non-empty")
	}
	if New(nil, false).Verify("anything") {
		t.Error("an empty key set must reject every token")
	}
}

func TestReplaceSwapsKeys(t *testing.T) {
	v := New([]string{"old"}, false)
	if !v.Verify("old") {
		t.Fatal("old key should verify before rotation")
	}
	v.Replace([]string{"new"}, false)
	if v.Verify("old") {
		t.Fatal("old key should stop working after rotation")
	}
	if !v.Verify("new") {
		t.Fatal("new key should verify after rotation")
	}
}

func TestExtractToken(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   string
	}{
		{"bearer", http.Header{"Authorization": {"Bearer abc"}}, "abc"},
		{"lowercase scheme", http.Header{"Authorization": {"bearer abc"}}, "abc"},
		{"bare value", http.Header{"Authorization": {"abc"}}, "abc"},
		{"x-api-key", http.Header{"X-Api-Key": {"def"}}, "def"},
		{"bearer wins over x-api-key", http.Header{"Authorization": {"Bearer abc"}, "X-Api-Key": {"def"}}, "abc"},
		{"absent", http.Header{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header = tt.header
			if got := ExtractToken(r); got != tt.want {
				t.Fatalf("ExtractToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMask(t *testing.T) {
	if got := Mask("sk-1234567890"); got != "sk-…890" {
		t.Errorf("Mask(long) = %q", got)
	}
	if got := Mask("short"); got != "****" {
		t.Errorf("Mask(short) = %q", got)
	}
}
