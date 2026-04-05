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

const nonceSize = 12

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
// DeriveKey.
func DeriveKeyV1(passphrase string) []byte {
	hash := sha256.Sum256([]byte(passphrase))
	return hash[:]
}

// DeriveKey derives a 32-byte key from a passphrase using argon2id.
// A deterministic salt is derived from the passphrase so that the same
// passphrase always produces the same key without storing a separate
// salt. The argon2id work factor provides brute-force protection.
func DeriveKey(passphrase string) []byte {
	salt := sha256.Sum256([]byte("pg_sage_kdf_v1:" + passphrase))
	return argon2.IDKey([]byte(passphrase), salt[:16], 1, 64*1024, 4, 32)
}

// DecryptWithMigration attempts to decrypt ciphertext with the current
// argon2id-derived key. If that fails, it falls back to the legacy
// SHA-256 key (v1). On a successful v1 decryption it returns the
// plaintext along with reEncrypted — the same plaintext re-encrypted
// under the new key — so the caller can update stored data.
//
// If needsReEncrypt is true the caller should persist reEncrypted in
// place of the original ciphertext.
func DecryptWithMigration(
	ciphertext []byte, passphrase string,
) (plaintext string, reEncrypted []byte, needsReEncrypt bool, err error) {
	newKey := DeriveKey(passphrase)
	plaintext, err = Decrypt(ciphertext, newKey)
	if err == nil {
		return plaintext, nil, false, nil
	}

	// Try legacy key.
	oldKey := DeriveKeyV1(passphrase)
	plaintext, err = Decrypt(ciphertext, oldKey)
	if err != nil {
		return "", nil, false, fmt.Errorf(
			"decrypt: failed with both v2 and v1 keys: %w", err)
	}

	// Re-encrypt under the new key.
	reEncrypted, err = Encrypt(plaintext, newKey)
	if err != nil {
		return "", nil, false, fmt.Errorf(
			"decrypt: re-encrypt after v1 migration: %w", err)
	}
	return plaintext, reEncrypted, true, nil
}
