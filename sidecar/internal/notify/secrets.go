package notify

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// SecretConfigKeys are channel config keys that hold credentials.
var SecretConfigKeys = []string{
	"webhook_url", "routing_key", "smtp_pass", "smtp_password",
	"api_key", "token", "secret", "signing_secret", "bot_token", "webhook_secret",
}

// sealedPrefix marks an AES-256-GCM sealed value (crypto.Encrypt,
// base64). Unprefixed values are legacy plaintext (G7-B20).
const sealedPrefix = "enc:v1:"

// SealSecrets returns a copy of cfg with every non-empty secret value
// encrypted under key. Already-sealed values are kept as is.
func SealSecrets(cfg map[string]string, key []byte) (map[string]string, error) {
	out := copyConfig(cfg)
	for _, k := range SecretConfigKeys {
		v, ok := out[k]
		if !ok || v == "" || strings.HasPrefix(v, sealedPrefix) {
			continue
		}
		ct, err := crypto.Encrypt(v, key)
		if err != nil {
			return nil, fmt.Errorf("sealing channel secret %s: %w", k, err)
		}
		out[k] = sealedPrefix + base64.StdEncoding.EncodeToString(ct)
	}
	return out, nil
}

// OpenSecrets returns a copy of cfg with sealed secrets decrypted.
// Legacy plaintext values pass through; a sealed value without a key
// (or with the wrong key) is an error, never silently empty.
func OpenSecrets(cfg map[string]string, key []byte) (map[string]string, error) {
	out := copyConfig(cfg)
	for _, k := range SecretConfigKeys {
		v := out[k]
		if !strings.HasPrefix(v, sealedPrefix) {
			continue
		}
		if len(key) == 0 {
			return nil, fmt.Errorf("channel secret %s is encrypted but "+
				"no encryption key is configured", k)
		}
		ct, err := base64.StdEncoding.DecodeString(v[len(sealedPrefix):])
		if err != nil {
			return nil, fmt.Errorf("decoding channel secret %s: %w", k, err)
		}
		pt, err := crypto.Decrypt(ct, key)
		if err != nil {
			return nil, fmt.Errorf("decrypting channel secret %s: %w", k, err)
		}
		out[k] = pt
	}
	return out, nil
}

// HasPlaintextSecrets reports whether any secret is stored unsealed.
func HasPlaintextSecrets(cfg map[string]string) bool {
	for _, k := range SecretConfigKeys {
		if v := cfg[k]; v != "" && !strings.HasPrefix(v, sealedPrefix) {
			return true
		}
	}
	return false
}

func copyConfig(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out
}
