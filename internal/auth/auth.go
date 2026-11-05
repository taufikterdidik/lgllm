// Package auth verifies inbound API keys.
//
// lgllm sits in front of paid upstream credentials, so the verifier is
// deliberately boring: constant-time comparison, no key material in logs, and a
// hash-only config form for shared or committed config files.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// Verifier checks presented tokens against the configured key set.
//
// The key set is replaced atomically on config reload, so requests in flight
// during a rotation keep using a consistent snapshot.
type Verifier struct {
	mu       sync.RWMutex
	disabled bool
	plain    [][]byte
	hashed   [][32]byte
}

// New builds a verifier from config entries.
//
// Each entry is either a raw key or "sha256:<hex>". Raw keys are convenient for
// a local config file; hashed entries keep the secret out of the file itself.
func New(entries []string, disabled bool) *Verifier {
	v := &Verifier{}
	v.Replace(entries, disabled)
	return v
}

// Replace swaps the key set. Safe for concurrent use with Verify.
func (v *Verifier) Replace(entries []string, disabled bool) {
	var plain [][]byte
	var hashed [][32]byte

	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if h, ok := strings.CutPrefix(e, "sha256:"); ok {
			raw, err := hex.DecodeString(strings.TrimSpace(h))
			if err != nil || len(raw) != sha256.Size {
				// A malformed digest must never turn into a usable key, nor
				// into an accidental empty-key match.
				continue
			}
			var sum [32]byte
			copy(sum[:], raw)
			hashed = append(hashed, sum)
			continue
		}
		plain = append(plain, []byte(e))
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.disabled = disabled
	v.plain = plain
	v.hashed = hashed
}

// Disabled reports whether key checking is turned off entirely.
func (v *Verifier) Disabled() bool {
	if v == nil {
		return true
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.disabled
}

// Empty reports whether no usable keys are configured.
func (v *Verifier) Empty() bool {
	if v == nil {
		return true
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.plain) == 0 && len(v.hashed) == 0
}

// Verify reports whether token matches any configured key.
//
// The comparison loop never short-circuits: timing must not reveal how many
// keys exist or which position matched.
func (v *Verifier) Verify(token string) bool {
	if v == nil || v.Disabled() {
		return true
	}
	if token == "" {
		return false
	}

	v.mu.RLock()
	plain, hashed, disabled := v.plain, v.hashed, v.disabled
	v.mu.RUnlock()

	if disabled {
		return true
	}
	if len(plain) == 0 && len(hashed) == 0 {
		return false
	}

	got := []byte(token)
	sum := sha256.Sum256(got)
	var ok int
	for _, k := range plain {
		ok |= subtle.ConstantTimeCompare(got, k)
	}
	for _, h := range hashed {
		ok |= subtle.ConstantTimeCompare(sum[:], h[:])
	}
	return ok == 1
}

// ExtractToken pulls the client credential from a request.
//
// Accepted shapes, in order of precedence:
//   - Authorization: Bearer <token>   (most [OI]-style CLIs)
//   - Authorization: <token>          (a few tools omit the scheme)
//   - x-api-key: <token>              (Anthropic SDKs)
func ExtractToken(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		if tok, found := strings.CutPrefix(h, "Bearer "); found {
			return strings.TrimSpace(tok)
		}
		if tok, found := strings.CutPrefix(h, "bearer "); found {
			return strings.TrimSpace(tok)
		}
		if !strings.ContainsAny(h, " \t") {
			return h
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// Mask returns a log-safe rendering of a token.
func Mask(token string) string {
	if len(token) <= 8 {
		return "****"
	}
	return token[:3] + "…" + token[len(token)-3:]
}
