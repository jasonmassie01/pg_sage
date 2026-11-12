package notify

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// SecretConfigKeys are channel config keys that hold credentials.
var SecretConfigKeys = []string{
	"webhook_url", "routing_key", "smtp_pass", "smtp_password",
	"api_key", "token", "secret", "signing_secret", "bot_token", "webhook_secret",
	"headers",   // webhook headers carry bearer tokens (alerting-06)
	"api_token", // a Jira ticket integration's API token (market-14)
}

// legacySealedPrefix marks a pre-security-08 value: crypto.Encrypt output
// in base64, bound to no channel. Reads still open it; startup re-seals it
// as sage-enc:v1. Unprefixed values are legacy plaintext (G7-B20).
const legacySealedPrefix = "enc:v1:"

// ChannelSecretAAD binds a sealed channel secret to its channel and key,
// so a value copied to another channel or key does not open there.
func ChannelSecretAAD(channelID int, key string) string {
	return fmt.Sprintf("sage.notification_channels|%d|%s", channelID, key)
}

// SealSecrets returns a copy of cfg with every non-empty plaintext secret
// sealed under kr's active key for channelID. Values that already carry a
// sealed prefix are kept as is; a nil kr keeps plaintext.
func SealSecrets(
	cfg map[string]string, kr *crypto.Keyring, channelID int,
) (map[string]string, error) {
	out := copyConfig(cfg)
	if kr == nil {
		return out, nil
	}
	for _, k := range SecretConfigKeys {
		v, ok := out[k]
		if !ok || v == "" || isSealedSecret(v) {
			continue
		}
		sealed, err := kr.Seal(v, ChannelSecretAAD(channelID, k))
		if err != nil {
			return nil, fmt.Errorf("sealing channel secret %s: %w", k, err)
		}
		out[k] = sealed
	}
	return out, nil
}

// OpenSecrets returns a copy of cfg with sealed secrets decrypted.
// Legacy plaintext values pass through; a sealed value without a key
// (or with the wrong key) is an error, never silently empty.
func OpenSecrets(
	cfg map[string]string, kr *crypto.Keyring, channelID int,
) (map[string]string, error) {
	out := copyConfig(cfg)
	for _, k := range SecretConfigKeys {
		v := out[k]
		if !isSealedSecret(v) {
			continue
		}
		if kr == nil {
			return nil, fmt.Errorf("channel secret %s is encrypted but "+
				"no encryption key is configured", k)
		}
		pt, err := openSecret(v, kr, ChannelSecretAAD(channelID, k))
		if errors.Is(err, crypto.ErrWrongEncryptionKey) {
			return nil, fmt.Errorf("channel secret %s does not open with "+
				"encryption_key; was encryption_key changed? re-enter the "+
				"secret or restore the old key: %w", k, err)
		}
		if err != nil {
			return nil, fmt.Errorf("decrypting channel secret %s: %w", k, err)
		}
		out[k] = pt
	}
	return out, nil
}

func openSecret(v string, kr *crypto.Keyring, aad string) (string, error) {
	if crypto.IsSealed(v) {
		pt, _, err := kr.Open(v, aad)
		if errors.Is(err, crypto.ErrOpenFailed) || errors.Is(err, crypto.ErrUnknownKeyID) {
			return "", fmt.Errorf("%w: %w", crypto.ErrWrongEncryptionKey, err)
		}
		return pt, err
	}
	ct, err := base64.StdEncoding.DecodeString(v[len(legacySealedPrefix):])
	if err != nil {
		return "", fmt.Errorf("decoding legacy value: %w", err)
	}
	pt, _, err := kr.OpenUnbound(ct)
	return pt, err
}

// NeedsReseal reports whether kr would store cfg differently: a plaintext
// or legacy enc:v1 secret, or one sealed under a key other than the active
// key. Without a keyring nothing can be sealed, so it is false.
func NeedsReseal(cfg map[string]string, kr *crypto.Keyring) bool {
	if kr == nil {
		return false
	}
	for _, k := range SecretConfigKeys {
		v := cfg[k]
		if v == "" {
			continue
		}
		kid, err := crypto.SealedKeyID(v)
		if err != nil || kid != kr.ActiveKeyID() {
			return true
		}
	}
	return false
}

// HasPlaintextSecrets reports whether any secret is stored with neither
// the sealed nor the legacy enc:v1 prefix.
func HasPlaintextSecrets(cfg map[string]string) bool {
	for _, k := range SecretConfigKeys {
		if v := cfg[k]; v != "" && !isSealedSecret(v) {
			return true
		}
	}
	return false
}

func isSealedSecret(v string) bool {
	return crypto.IsSealed(v) || strings.HasPrefix(v, legacySealedPrefix)
}

func copyConfig(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out
}
