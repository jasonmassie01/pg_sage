package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SealedPrefix marks a value sealed by a Keyring. The full format is
// "sage-enc:v1:<key id>:<base64url(nonce || ciphertext || tag)>".
const SealedPrefix = "sage-enc:v1:"

var (
	// ErrNotSealed means the value does not carry SealedPrefix.
	ErrNotSealed = errors.New("crypto: value is not sealed")
	// ErrMalformedSealed means the value has the prefix but no valid body.
	ErrMalformedSealed = errors.New("crypto: malformed sealed value")
	// ErrUnknownKeyID means no key in the keyring has the value's key id.
	ErrUnknownKeyID = errors.New("crypto: sealed value's key id is not configured")
	// ErrOpenFailed means authentication failed: wrong key, wrong
	// associated data or a tampered value.
	ErrOpenFailed = errors.New("crypto: sealed value failed authentication")
)

// Keyring seals values with AES-256-GCM under its active key and opens
// values sealed under any of its keys. Each sealed value names its key id,
// so a rotated keyring still opens values sealed under a previous key.
// A Keyring is immutable and safe for concurrent use.
type Keyring struct {
	activeID string
	aeads    map[string]cipher.AEAD
}

// KeyIDFor returns the public identifier of a key: 16 hex characters of a
// domain-separated SHA-256 of the key. It does not reveal the key.
func KeyIDFor(material []byte) string {
	h := sha256.New()
	h.Write([]byte("pg_sage key id v1\x00"))
	h.Write(material)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// NewKeyring builds a keyring whose active (sealing) key is active and
// which also opens values sealed under previous. Keys are 32 bytes; empty
// previous entries are skipped so callers can pass an unset previous key.
func NewKeyring(active []byte, previous ...[]byte) (*Keyring, error) {
	kr := &Keyring{aeads: make(map[string]cipher.AEAD, 1+len(previous))}
	id, err := kr.add(active)
	if err != nil {
		return nil, fmt.Errorf("keyring: active key: %w", err)
	}
	kr.activeID = id
	for i, key := range previous {
		if len(key) == 0 {
			continue
		}
		if _, err := kr.add(key); err != nil {
			return nil, fmt.Errorf("keyring: previous key %d: %w", i+1, err)
		}
	}
	return kr, nil
}

func (k *Keyring) add(key []byte) (string, error) {
	if len(key) != 32 {
		return "", fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("creating cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("creating GCM: %w", err)
	}
	id := KeyIDFor(key)
	k.aeads[id] = aead
	return id, nil
}

// ActiveKeyID is the id new values are sealed under ("" for nil).
func (k *Keyring) ActiveKeyID() string {
	if k == nil {
		return ""
	}
	return k.activeID
}

// Seal encrypts plaintext under the active key, binding aad (which must be
// supplied again to Open) so a sealed value cannot be moved to another
// context.
func (k *Keyring) Seal(plaintext, aad string) (string, error) {
	if k == nil {
		return "", errors.New("crypto: seal with no keyring")
	}
	aead := k.aeads[k.activeID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: generating nonce: %w", err)
	}
	body := aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return SealedPrefix + k.activeID + ":" +
		base64.RawURLEncoding.EncodeToString(body), nil
}

// Open decrypts a sealed value and returns its plaintext and the id of
// the key that sealed it.
func (k *Keyring) Open(sealed, aad string) (string, string, error) {
	kid, body, err := parseSealed(sealed)
	if err != nil {
		return "", "", err
	}
	if k == nil {
		return "", "", fmt.Errorf("%w: no keyring (key id %s)", ErrUnknownKeyID, kid)
	}
	aead, ok := k.aeads[kid]
	if !ok {
		return "", "", fmt.Errorf("%w: %s", ErrUnknownKeyID, kid)
	}
	if len(body) < aead.NonceSize()+aead.Overhead() {
		return "", "", fmt.Errorf("%w: too short", ErrMalformedSealed)
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, body[:n], body[n:], []byte(aad))
	if err != nil {
		return "", "", fmt.Errorf("%w (key id %s)", ErrOpenFailed, kid)
	}
	return string(plain), kid, nil
}

// IsSealed reports whether v carries the sealed-value prefix.
func IsSealed(v string) bool {
	return strings.HasPrefix(v, SealedPrefix)
}

// SealedKeyID returns the key id a sealed value names.
func SealedKeyID(v string) (string, error) {
	kid, _, err := parseSealed(v)
	return kid, err
}

func parseSealed(v string) (string, []byte, error) {
	if !IsSealed(v) {
		return "", nil, ErrNotSealed
	}
	kid, encoded, ok := strings.Cut(strings.TrimPrefix(v, SealedPrefix), ":")
	if !ok || kid == "" || encoded == "" {
		return "", nil, fmt.Errorf("%w: missing key id or body", ErrMalformedSealed)
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", nil, fmt.Errorf("%w: body is not base64url", ErrMalformedSealed)
	}
	return kid, body, nil
}
