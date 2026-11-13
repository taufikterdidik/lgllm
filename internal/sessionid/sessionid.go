// Package sessionid creates stable, opaque conversation identifiers for
// upstream providers that require a session header.
//
// A provider such as OpenCode Go uses the id for routing and prompt-cache
// affinity. It must be stable across the turns of one conversation and unique
// enough between conversations; the value itself is opaque.
package sessionid

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// MaxLength caps the value sent in a header. Real ids are 30 characters; a
// large value is a sign that a caller passed a body by mistake.
const MaxLength = 256

// Sanitize trims a client-supplied id and removes bytes that are unsafe in an
// HTTP header value. It never invents a value: an empty result means the caller
// should use a fallback.
func Sanitize(v string) string {
	v = strings.TrimSpace(v)
	var b strings.Builder
	for _, r := range v {
		// Keep visible ASCII and the space between words; drop controls and
		// non-ASCII so a header can never be split or mis-encoded.
		if r < 0x20 || r == 0x7f || r > 0x7e {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= MaxLength {
			break
		}
	}
	return b.String()
}

// Fingerprint derives a stable id from a conversation seed.
//
// The shape matches OpenCode's canonical session ids (ses_ + 12 hex + 14
// alphanumeric characters), but providers treat the value as opaque. Using the
// same seed for every turn of a conversation keeps prompt-cache affinity.
func Fingerprint(seed string) string {
	sum := sha256.Sum256([]byte(seed))

	var b strings.Builder
	b.Grow(30)
	b.WriteString("ses_")
	b.WriteString(hex.EncodeToString(sum[:6]))

	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for i := 0; i < 14; i++ {
		b.WriteByte(alphabet[int(sum[6+i])%len(alphabet)])
	}
	return b.String()
}
