package crypto

import (
	"errors"
	"fmt"
)

// ErrWrongEncryptionKey means no configured key opens a sealed value: the
// value was sealed under a different encryption_key (or was altered).
var ErrWrongEncryptionKey = errors.New("encryption key does not open this value")

// LegacyKeys are the pre-v0.9 keys derived from a passphrase: V2 is the
// deterministic-salt argon2id key and V1 the SHA-256 key. The zero value
// means legacy migration is off.
type LegacyKeys struct {
	V2, V1 []byte
}

// DeriveLegacyKeys derives both legacy keys once (one argon2id run), so
// reads can try them without deriving anything. An empty passphrase gives
// the zero value.
func DeriveLegacyKeys(passphrase string) LegacyKeys {
	if passphrase == "" {
		return LegacyKeys{}
	}
	return LegacyKeys{
		V2: DeriveKeyV2Legacy(passphrase),
		V1: DeriveKeyV1(passphrase),
	}
}

// IsZero reports whether no legacy key is set.
func (k LegacyKeys) IsZero() bool { return len(k.V2) == 0 && len(k.V1) == 0 }

// DecryptLegacy opens ciphertext with the current key, then with the legacy
// keys. It derives nothing. When a legacy key opens the value, resealed is
// the plaintext sealed under current, for the caller to persist. When no
// key opens it, the error wraps ErrWrongEncryptionKey. Other errors (a
// truncated value, a malformed key) are returned as is.
func DecryptLegacy(
	ciphertext, current []byte, legacy LegacyKeys,
) (plaintext string, resealed []byte, err error) {
	plaintext, err = Decrypt(ciphertext, current)
	if !errors.Is(err, ErrWrongEncryptionKey) {
		return plaintext, nil, err
	}
	for _, key := range [][]byte{legacy.V2, legacy.V1} {
		if len(key) == 0 {
			continue
		}
		pt, legacyErr := Decrypt(ciphertext, key)
		if errors.Is(legacyErr, ErrWrongEncryptionKey) {
			continue
		}
		if legacyErr != nil {
			return "", nil, legacyErr
		}
		resealed, err = Encrypt(pt, current)
		if err != nil {
			return "", nil, fmt.Errorf("decrypt: re-sealing a legacy value: %w", err)
		}
		return pt, resealed, nil
	}
	return "", nil, err
}
