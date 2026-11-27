package auditchain_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// No concurrent access tests here: signing and verifying are pure
// functions of their arguments.

func testSigner(t *testing.T, id string) auditchain.CheckpointSigner {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return auditchain.CheckpointSigner{ID: id, Key: key}
}

var headHash = strings.Repeat("ab", 32)

func TestSignedCheckpointVerifiesWithPublishedKey(t *testing.T) {
	s := testSigner(t, "k1")
	cp, err := auditchain.SignCheckpoint(s, "action_log", 42, headHash)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if cp.Chain != "action_log" || cp.Seq != 42 || cp.Hash != headHash || cp.KeyID != "k1" ||
		cp.Alg != "ed25519" || len(cp.Signature) != 2*ed25519.SignatureSize {
		t.Fatalf("signed checkpoint = %+v", cp)
	}
	if err := cp.Verify(s.Trusted()); err != nil {
		t.Fatalf("verify with the signer's public key: %v", err)
	}
	if got := s.Trusted(); got.ID != "k1" || !got.Public.Equal(s.Key.Public()) {
		t.Fatalf("trusted key = %+v", got)
	}
}

// One changed byte in any signed field, or in the signature, fails.
func TestSignedCheckpointFailsAfterOneByteChange(t *testing.T) {
	s := testSigner(t, "k1")
	cp, err := auditchain.SignCheckpoint(s, "action_log", 42, headHash)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	flip := func(v string) string {
		b := []byte(v)
		if b[0] == 'a' {
			b[0] = 'b'
		} else {
			b[0] = 'a'
		}
		return string(b)
	}
	cases := map[string]func(c *auditchain.SignedCheckpoint){
		"signature": func(c *auditchain.SignedCheckpoint) { c.Signature = flip(c.Signature) },
		"hash":      func(c *auditchain.SignedCheckpoint) { c.Hash = flip(c.Hash) },
		"seq":       func(c *auditchain.SignedCheckpoint) { c.Seq = 43 },
		"chain":     func(c *auditchain.SignedCheckpoint) { c.Chain = "auth_audit" },
	}
	for name, edit := range cases {
		c := cp
		edit(&c)
		if err := c.Verify(s.Trusted()); !errors.Is(err, auditchain.ErrCheckpointSignature) {
			t.Errorf("%s changed: err = %v, want ErrCheckpointSignature", name, err)
		}
	}
	if err := cp.Verify(s.Trusted()); err != nil {
		t.Fatalf("the unchanged checkpoint no longer verifies: %v", err)
	}
}

func TestSignedCheckpointRefusesOtherKeys(t *testing.T) {
	s, other := testSigner(t, "k1"), testSigner(t, "k2")
	cp, err := auditchain.SignCheckpoint(s, "action_log", 1, headHash)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := cp.Verify(other.Trusted()); !errors.Is(err, auditchain.ErrCheckpointSignature) ||
		!strings.Contains(err.Error(), "k1") || !strings.Contains(err.Error(), "k2") {
		t.Fatalf("verify under another key id: %v", err)
	}
	// Another key presented under the signer's id still fails on the signature.
	impostor := auditchain.TrustedKey{ID: "k1", Public: other.Trusted().Public}
	if err := cp.Verify(impostor); !errors.Is(err, auditchain.ErrCheckpointSignature) {
		t.Fatalf("verify with another key under the same id: %v", err)
	}
}

func TestSignCheckpointInvalidInput(t *testing.T) {
	s := testSigner(t, "k1")
	cases := []struct {
		name   string
		signer auditchain.CheckpointSigner
		chain  string
		seq    int64
		hash   string
	}{
		{"no key", auditchain.CheckpointSigner{ID: "k1"}, "action_log", 1, headHash},
		{"no key id", auditchain.CheckpointSigner{Key: s.Key}, "action_log", 1, headHash},
		{"bad chain", s, "Action Log", 1, headHash},
		{"zero seq", s, "action_log", 0, headHash},
		{"negative seq", s, "action_log", -1, headHash},
		{"short hash", s, "action_log", 1, "abc"},
		{"non-hex hash", s, "action_log", 1, strings.Repeat("zz", 32)},
	}
	for _, c := range cases {
		if _, err := auditchain.SignCheckpoint(c.signer, c.chain, c.seq, c.hash); err == nil {
			t.Errorf("%s: signed, want an error", c.name)
		}
	}
}

func TestSignedCheckpointVerifyNilAndMalformed(t *testing.T) {
	s := testSigner(t, "k1")
	cp, err := auditchain.SignCheckpoint(s, "action_log", 5, headHash)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := cp.Verify(auditchain.TrustedKey{ID: "k1"}); err == nil ||
		!strings.Contains(err.Error(), "public key") {
		t.Fatalf("verify without a public key: %v", err)
	}
	bad := cp
	bad.Signature = "not hex"
	if err := bad.Verify(s.Trusted()); !errors.Is(err, auditchain.ErrCheckpointSignature) {
		t.Fatalf("verify a non-hex signature: %v", err)
	}
	bad = cp
	bad.Alg = "rsa"
	if err := bad.Verify(s.Trusted()); !errors.Is(err, auditchain.ErrCheckpointSignature) ||
		!strings.Contains(err.Error(), "rsa") {
		t.Fatalf("verify another algorithm: %v", err)
	}
	var zero auditchain.SignedCheckpoint
	if err := zero.Verify(s.Trusted()); !errors.Is(err, auditchain.ErrCheckpointSignature) {
		t.Fatalf("verify the zero checkpoint: %v", err)
	}
}

// The link payload round-trips: what AppendSignedCheckpoint stores is what
// the verifier parses back.
func TestSignedCheckpointPayloadRoundTrip(t *testing.T) {
	s := testSigner(t, "k1")
	cp, err := auditchain.SignCheckpoint(s, "auth_audit", 9, headHash)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	payload, err := cp.Payload()
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	back, err := auditchain.ParseSignedCheckpoint(payload)
	if err != nil || back != cp {
		t.Fatalf("parse(payload) = %+v, %v; want %+v", back, err, cp)
	}
	for _, bad := range []string{"", "{", `{"chain":"auth_audit"}`, "[]"} {
		if _, err := auditchain.ParseSignedCheckpoint(bad); err == nil {
			t.Errorf("parse %q: no error", bad)
		}
	}
}
