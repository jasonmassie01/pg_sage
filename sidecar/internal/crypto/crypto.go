package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
)

// argon2Calls counts argon2id derivations; each costs 64 MiB and tens of
// milliseconds, so hot paths must not run one per read.
var argon2Calls atomic.Int64

// Argon2Derivations returns how many argon2id derivations this process has
// run. Tests and benchmarks use it to prove a path derives nothing.
func Argon2Derivations() int64 { return argon2Calls.Load() }

// idKey is the only caller of argon2.IDKey, so every derivation is counted.
func idKey(passphrase string, salt []byte) []byte {
	argon2Calls.Add(1)
	return argon2.IDKey([]byte(passphrase), salt, 1, 64*1024, 4, 32)
}

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
		// GCM cannot tell a different key from a tampered value; with
		// sealed rows only pg_sage writes, a different key is the cause.
		return "", fmt.Errorf("decrypt: %w: %w", ErrWrongEncryptionKey, err)
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
	return idKey(passphrase, salt[:16])
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
	return idKey(passphrase, salt)
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
