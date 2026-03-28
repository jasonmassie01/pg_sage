package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
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

// DeriveKey derives a 32-byte key from a passphrase using SHA-256.
// Used when the user provides a string key instead of raw bytes.
func DeriveKey(passphrase string) []byte {
	hash := sha256.Sum256([]byte(passphrase))
	return hash[:]
}
