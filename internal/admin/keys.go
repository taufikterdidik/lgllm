package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"lgllm/internal/config"
)

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
	tmp := &config.Config{
		Providers:   st.Providers,
		Connections: st.Connections,
		Auth:        config.Auth{Keys: st.Keys},
	}
	if err := config.DecryptSecrets(tmp, masterKey); err != nil {
		return err
	}
	st.Providers = tmp.Providers
	st.Connections = tmp.Connections
	st.Keys = tmp.Auth.Keys
	return nil
}
