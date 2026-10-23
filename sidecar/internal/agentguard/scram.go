package agentguard

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// scramIterations is PostgreSQL's default scram_iterations.
const scramIterations = 4096

const (
	scramSaltBytes = 16
	secretBytes    = 32
)

// newBrokerSecret returns a random broker password: 32 random bytes,
// base64url without padding (43 characters, no SASLprep surprises).
func newBrokerSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("agentguard: generating broker secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// scramVerifier computes PostgreSQL's SCRAM-SHA-256 verifier for password
// client-side (RFC 5802, RFC 7677), so CREATE ROLE … PASSWORD carries the
// verifier and the plaintext never reaches the server or its logs:
// SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>.
func scramVerifier(password string, salt []byte, iterations int) (string, error) {
	if len(salt) == 0 || iterations <= 0 {
		return "", invalid("SCRAM salt and iteration count are required")
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("agentguard: deriving SCRAM key: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	enc := base64.StdEncoding
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, enc.EncodeToString(salt),
		enc.EncodeToString(storedKey[:]), enc.EncodeToString(serverKey)), nil
}

// newScramVerifier is scramVerifier with a fresh random salt.
func newScramVerifier(password string) (string, error) {
	salt := make([]byte, scramSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("agentguard: generating SCRAM salt: %w", err)
	}
	return scramVerifier(password, salt, scramIterations)
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
