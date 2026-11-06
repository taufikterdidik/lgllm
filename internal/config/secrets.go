package config

import (
	"fmt"

	"lgllm/internal/secret"
)

// DecryptSecrets replaces "enc:v1:" values with plaintext in memory.
//
// The file is never rewritten, so an encrypted config stays encrypted on disk.
// If a value is encrypted and no master key is configured, loading fails: using
// the literal ciphertext as a credential would only produce confusing upstream
// 401s.
func DecryptSecrets(c *Config, masterKey string) error {
	var box *secret.Box
	if masterKey != "" {
		var err error
		box, err = secret.New(masterKey)
		if err != nil {
			return fmt.Errorf("master key: %w", err)
		}
	}

	decrypt := func(where, value string) (string, error) {
		if !secret.IsEncrypted(value) {
			return value, nil
		}
		if box == nil {
			return "", fmt.Errorf("%s is encrypted but no master key is configured (set LGLLM_MASTER_KEY or --master-key)", where)
		}
		plain, err := box.Decrypt(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", where, err)
		}
		return plain, nil
	}

	for i := range c.Connections {
		v, err := decrypt(fmt.Sprintf("connections[%d].api_key", i), c.Connections[i].APIKey)
		if err != nil {
			return err
		}
		c.Connections[i].APIKey = v
	}

	for i := range c.Auth.Keys {
		v, err := decrypt(fmt.Sprintf("auth.keys[%d]", i), c.Auth.Keys[i])
		if err != nil {
			return err
		}
		c.Auth.Keys[i] = v
	}

	for i := range c.Providers {
		for key, value := range c.Providers[i].Headers {
			v, err := decrypt(fmt.Sprintf("providers[%d].headers[%q]", i, key), value)
			if err != nil {
				return err
			}
			c.Providers[i].Headers[key] = v
		}
	}
	return nil
}
