package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

const (
	nonceSize = 12
	// KDFSaltSize is the byte length of the per-deployment argon2id
	// salt persisted in sage.crypto_meta.
	KDFSaltSize = 16
)

// Encrypt encrypts plaintext using AES-256-GCM with the provided key.
// Key must be 32 bytes. Returns ciphertext with 12-byte nonce prepended.
func Encrypt(plaintext string, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encrypt: key must be 32 bytes, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encrypt: creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt: creating GCM: %w", err)
	}

	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encrypt: generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return ciphertext, nil
}

// Decrypt decrypts AES-256-GCM ciphertext with the provided key.
// Expects a 12-byte nonce prepended to the ciphertext.
// Returns the original plaintext string.
func Decrypt(ciphertext []byte, key []byte) (string, error) {
	if len(key) != 32 {
		return "", fmt.Errorf("decrypt: key must be 32 bytes, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("decrypt: creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt: creating GCM: %w", err)
	}

	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("decrypt: ciphertext too short")
	}

	nonce := ciphertext[:nonceSize]
	encrypted := ciphertext[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}

// DeriveKeyV1 derives a 32-byte key from a passphrase using SHA-256.
// Deprecated: retained only for backward-compatible decryption of data
// encrypted before the argon2id migration. New encryptions must use
// DeriveKey with a per-deployment random salt.
func DeriveKeyV1(passphrase string) []byte {
	hash := sha256.Sum256([]byte(passphrase))
	return hash[:]
}

// DeriveKeyV2Legacy derives a 32-byte key from a passphrase using
// argon2id with a deterministic passphrase-derived salt.
//
// Deprecated: retained only for backward-compatible decryption of data
// encrypted before the per-deployment-random-salt migration. The
// deterministic salt made precomputed dictionary attacks cheaper
// because every deployment sharing a passphrase produced the same
// key. New encryptions must use DeriveKey with a random salt read
// from sage.crypto_meta.
func DeriveKeyV2Legacy(passphrase string) []byte {
	salt := sha256.Sum256([]byte("pg_sage_kdf_v1:" + passphrase))
	return argon2.IDKey(
		[]byte(passphrase), salt[:16], 1, 64*1024, 4, 32,
	)
}

// DeriveKey derives a 32-byte key from a passphrase using argon2id
// with the supplied per-deployment random salt. The salt must be
// stable across restarts (persisted in sage.crypto_meta) so that keys
// derived at startup match the keys used to encrypt prior records.
//
// A random per-deployment salt prevents attackers with stolen
// ciphertexts from amortizing dictionary attacks across multiple
// pg_sage deployments that share a common passphrase.
func DeriveKey(passphrase string, salt []byte) []byte {
	if len(salt) < 8 {
		// Enforce a minimum salt length to catch accidental zero-
		// length salts; argon2.IDKey panics on nil so we want to
		// fail loudly with a clear message instead.
		panic(fmt.Sprintf(
			"crypto.DeriveKey: salt too short (%d bytes); "+
				"expected >= 8", len(salt)))
	}
	return argon2.IDKey([]byte(passphrase), salt, 1, 64*1024, 4, 32)
}

// NewKDFSalt generates a cryptographically random KDFSaltSize-byte
// salt for persisting in sage.crypto_meta at first bootstrap.
func NewKDFSalt() ([]byte, error) {
	b := make([]byte, KDFSaltSize)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("crypto: generating kdf salt: %w", err)
	}
	return b, nil
}

// DecryptWithMigration attempts to decrypt ciphertext using the current
// salted argon2id key. If that fails it falls back, in order, to the
// legacy deterministic-salt argon2id key and then to the original
// SHA-256 key. On a successful legacy decryption the plaintext is
// returned along with reEncrypted — the same plaintext re-encrypted
// under the current key — so the caller can persist the upgrade.
//
// If needsReEncrypt is true the caller must persist reEncrypted in
// place of the original ciphertext.
func DecryptWithMigration(
	ciphertext []byte, passphrase string, salt []byte,
) (plaintext string, reEncrypted []byte, needsReEncrypt bool, err error) {
	newKey := DeriveKey(passphrase, salt)
	plaintext, err = Decrypt(ciphertext, newKey)
	if err == nil {
		return plaintext, nil, false, nil
	}

	// Try deterministic-salt argon2id (v2 legacy).
	v2Key := DeriveKeyV2Legacy(passphrase)
	plaintext, err = Decrypt(ciphertext, v2Key)
	if err == nil {
		reEncrypted, encErr := Encrypt(plaintext, newKey)
		if encErr != nil {
			return "", nil, false, fmt.Errorf(
				"decrypt: re-encrypt after v2 migration: %w", encErr)
		}
		return plaintext, reEncrypted, true, nil
	}

	// Try SHA-256 (v1 legacy).
	v1Key := DeriveKeyV1(passphrase)
	plaintext, err = Decrypt(ciphertext, v1Key)
	if err != nil {
		return "", nil, false, fmt.Errorf(
			"decrypt: failed with v3, v2, and v1 keys: %w", err)
	}

	// Re-encrypt under the current key.
	reEncrypted, err = Encrypt(plaintext, newKey)
	if err != nil {
		return "", nil, false, fmt.Errorf(
			"decrypt: re-encrypt after v1 migration: %w", err)
	}
	return plaintext, reEncrypted, true, nil
}
