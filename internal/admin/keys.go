package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"

	"lgllm/internal/config"
)

const maxKeyNameLength = 64

// GenerateKey returns a new client key. The prefix makes it easy to recognize in
// logs and paste dialogs; the random part is 256 bits from crypto/rand.
func GenerateKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return "sk-lgllm-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// normalizeKeyName trims and validates a human label for a client key.
func normalizeKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if len([]rune(name)) > maxKeyNameLength {
		return "", fmt.Errorf("key name must be at most %d characters", maxKeyNameLength)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("key name must not contain control characters")
		}
	}
	return name, nil
}

func keyNameTaken(keys []config.AdminKey, name, skipID string) bool {
	if name == "" {
		return false
	}
	for _, key := range keys {
		if KeyID(key.Key) == skipID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key.Name), name) {
			return true
		}
	}
	return false
}

// KeyID is the non-reversible identifier returned by GET /api/keys.
//
// The raw client key is never returned; a truncated SHA-256 is enough to address
// an admin-managed key for deletion.
func KeyID(key string) string {
	if digest, ok := strings.CutPrefix(key, "sha256:"); ok {
		if len(digest) > 16 {
			digest = digest[:16]
		}
		return "sha256:" + digest
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// decryptState opens encrypted secrets in the overlay so the manager works with
// plaintext in memory, exactly like the running config.
func decryptState(st *config.AdminState, masterKey string) error {
	keys := make([]string, len(st.Keys))
	for i := range st.Keys {
		keys[i] = st.Keys[i].Key
	}
	tmp := &config.Config{
		Providers:   st.Providers,
		Connections: st.Connections,
		Auth:        config.Auth{Keys: keys},
	}
	if err := config.DecryptSecrets(tmp, masterKey); err != nil {
		return err
	}
	st.Providers = tmp.Providers
	st.Connections = tmp.Connections
	for i := range st.Keys {
		st.Keys[i].Key = tmp.Auth.Keys[i]
	}
	return nil
}
