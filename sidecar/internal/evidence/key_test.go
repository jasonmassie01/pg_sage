package evidence

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

// No concurrent access tests: the public-key helpers and seal are pure
// functions of their arguments.

func pemOf(t *testing.T, typ string, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// Happy path: the served PEM and the manifest's hex both parse back to the
// same key.
func TestPublicKeyRoundTrip(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	text, err := MarshalPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.HasPrefix(text, "-----BEGIN PUBLIC KEY-----") {
		t.Fatalf("not a PKIX PEM: %q", text)
	}
	for name, in := range map[string][]byte{"pem": []byte(text),
		"hex": []byte(hex.EncodeToString(pub) + "\n")} {
		got, err := ParsePublicKey(in)
		if err != nil || !got.Equal(pub) {
			t.Fatalf("%s: got %x err %v, want %x", name, got, err, pub)
		}
	}
}

// Invalid input: other key types are refused, naming the type; so are
// private keys, junk and a hex key of the wrong length.
func TestParsePublicKeyRefusals(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	rsaDER, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	privPEM, _ := MarshalSigningKey(priv)
	cases := map[string]struct {
		in   []byte
		want string
	}{
		"ecdsa":     {pemOf(t, "PUBLIC KEY", ecDER), "ECDSA"},
		"rsa":       {pemOf(t, "PUBLIC KEY", rsaDER), "RSA"},
		"private":   {[]byte(privPEM), "PRIVATE KEY"},
		"junk":      {[]byte("not a key"), "PEM"},
		"empty":     {nil, "PEM"},
		"short hex": {[]byte("abcd"), "32"},
	}
	for name, c := range cases {
		got, err := ParsePublicKey(c.in)
		if !errors.Is(err, ErrPublicKey) || got != nil {
			t.Fatalf("%s: key %x err %v, want ErrPublicKey", name, got, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %q does not name %q", name, err, c.want)
		}
	}
}

// Nil/empty: an unsigned manifest says so and what its hashes are worth;
// a signed one says it is signed and carries no such caveat.
func TestManifestIntegrityMarker(t *testing.T) {
	unsigned := &Pack{meta: map[string]fileMeta{}}
	unsigned.seal()
	if unsigned.Manifest.Signed || unsigned.Manifest.Integrity != IntegrityHashOnly ||
		IntegrityHashOnly != "accidental-corruption-only" {
		t.Fatalf("unsigned manifest: signed=%v integrity=%q", unsigned.Manifest.Signed,
			unsigned.Manifest.Integrity)
	}
	raw, _ := json.Marshal(unsigned.Manifest)
	if !strings.Contains(string(raw), `"signed":false`) ||
		!strings.Contains(string(raw), `"integrity":"accidental-corruption-only"`) {
		t.Fatalf("unsigned manifest JSON lacks the marker: %s", raw)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signed := &Pack{key: priv, meta: map[string]fileMeta{}}
	signed.seal()
	raw, _ = json.Marshal(signed.Manifest)
	if !signed.Manifest.Signed || signed.Manifest.Integrity != "" ||
		strings.Contains(string(raw), "integrity") || signed.Manifest.Signer == nil {
		t.Fatalf("signed manifest: %s", raw)
	}
}
