package evidence

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// ErrPublicKey means a trusted key is not an Ed25519 public key.
var ErrPublicKey = errors.New("evidence: not an Ed25519 public key")

// MarshalPublicKey renders pub as a PKIX "PUBLIC KEY" PEM, the form
// GET /api/v1/audit/evidence/key serves and openssl pkeyutl reads.
func MarshalPublicKey(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("evidence: marshal public key: %w", err)
	}
	var b bytes.Buffer
	if err := pem.Encode(&b, &pem.Block{Type: "PUBLIC KEY", Bytes: der}); err != nil {
		return "", fmt.Errorf("evidence: marshal public key: %w", err)
	}
	return b.String(), nil
}

// ParsePublicKey reads a trusted Ed25519 public key: a PKIX PEM, or the 64
// hex characters a manifest's signer.public_key holds.
func ParsePublicKey(data []byte) (ed25519.PublicKey, error) {
	text := strings.TrimSpace(string(data))
	if block, _ := pem.Decode([]byte(text)); block != nil {
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: the PEM block is %q, want a PUBLIC KEY: %v",
				ErrPublicKey, block.Type, err)
		}
		ed, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: the key is %s", ErrPublicKey, keyTypeName(key))
		}
		return ed, nil
	}
	raw, err := hex.DecodeString(text)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: want a PEM public key or %d bytes of hex",
			ErrPublicKey, ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

func keyTypeName(key any) string {
	switch key.(type) {
	case *rsa.PublicKey:
		return "an RSA key"
	case *ecdsa.PublicKey:
		return "an ECDSA key"
	case *ecdh.PublicKey:
		return "an ECDH key"
	}
	return fmt.Sprintf("a %T", key)
}
