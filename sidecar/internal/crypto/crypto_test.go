package crypto

import (
	"bytes"
	"strings"
	"testing"
)

// testSalt is a deterministic per-test salt. Using a fixed value keeps
// DeriveKey deterministic across test runs so key-equality assertions
// hold. Production code reads a random salt from sage.crypto_meta.
var testSalt = []byte("unit-test-salt16")

func TestEncryptDecrypt(t *testing.T) {
	key := DeriveKey("test-passphrase", testSalt)
	plaintext := "hello, pg_sage!"

	ciphertext, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := Decrypt(ciphertext, key)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if got != plaintext {
		t.Errorf("round-trip failed: got %q, want %q", got, plaintext)
	}
}

func TestEncryptDifferentNonce(t *testing.T) {
	key := DeriveKey("test-passphrase", testSalt)
	plaintext := "same input"

	ct1, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt (1): %v", err)
	}

	ct2, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt (2): %v", err)
	}

	if bytes.Equal(ct1, ct2) {
		t.Error("same plaintext produced identical ciphertext")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key1 := DeriveKey("correct-key", testSalt)
	key2 := DeriveKey("wrong-key", testSalt)

	ciphertext, err := Encrypt("secret", key1)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	_, err = Decrypt(ciphertext, key2)
	if err == nil {
		t.Error("Decrypt with wrong key should fail")
	}
}

func TestDecryptCorruptedData(t *testing.T) {
	key := DeriveKey("test-key", testSalt)

	ciphertext, err := Encrypt("hello", key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Corrupt a byte in the encrypted portion (after nonce).
	if len(ciphertext) > nonceSize {
		ciphertext[nonceSize] ^= 0xFF
	}

	_, err = Decrypt(ciphertext, key)
	if err == nil {
		t.Error("Decrypt with corrupted data should fail")
	}
}

func TestDeriveKey(t *testing.T) {
	k1 := DeriveKey("my-passphrase", testSalt)
	k2 := DeriveKey("my-passphrase", testSalt)

	if !bytes.Equal(k1, k2) {
		t.Error("DeriveKey is not deterministic for a fixed salt")
	}
}

// TestDeriveKey_DifferentSaltsDiffer ensures the per-deployment salt
// actually varies the derived key. If this ever fails, two separate
// deployments would share keys — defeating the whole point of the
// salt.
func TestDeriveKey_DifferentSaltsDiffer(t *testing.T) {
	saltA := []byte("deployment-A-xyz")
	saltB := []byte("deployment-B-xyz")
	k1 := DeriveKey("my-passphrase", saltA)
	k2 := DeriveKey("my-passphrase", saltB)
	if bytes.Equal(k1, k2) {
		t.Error("Different salts must produce different keys")
	}
}

func TestDeriveKeyLength(t *testing.T) {
	key := DeriveKey("any-passphrase", testSalt)
	if len(key) != 32 {
		t.Errorf("DeriveKey length = %d, want 32", len(key))
	}
}

// TestDeriveKey_RejectsShortSalt verifies the runtime guard against
// nil/empty salts. Without this, argon2.IDKey panics deep in the
// stack — we want a clear failure at the API boundary.
func TestDeriveKey_RejectsShortSalt(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on short salt")
		}
	}()
	_ = DeriveKey("passphrase", []byte("short"))
}

func TestNewKDFSalt_LengthAndRandomness(t *testing.T) {
	a, err := NewKDFSalt()
	if err != nil {
		t.Fatalf("NewKDFSalt: %v", err)
	}
	if len(a) != KDFSaltSize {
		t.Errorf("salt length = %d, want %d", len(a), KDFSaltSize)
	}
	b, err := NewKDFSalt()
	if err != nil {
		t.Fatalf("NewKDFSalt: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("two generated salts should not collide")
	}
}

func TestEmptyPlaintext(t *testing.T) {
	key := DeriveKey("test-key", testSalt)

	ciphertext, err := Encrypt("", key)
	if err != nil {
		t.Fatalf("Encrypt empty: %v", err)
	}

	got, err := Decrypt(ciphertext, key)
	if err != nil {
		t.Fatalf("Decrypt empty: %v", err)
	}

	if got != "" {
		t.Errorf("empty round-trip: got %q, want empty", got)
	}
}

func TestLongPlaintext(t *testing.T) {
	key := DeriveKey("test-key", testSalt)
	plaintext := strings.Repeat("A", 10000)

	ciphertext, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt long: %v", err)
	}

	got, err := Decrypt(ciphertext, key)
	if err != nil {
		t.Fatalf("Decrypt long: %v", err)
	}

	if got != plaintext {
		t.Errorf("long round-trip: got length %d, want %d",
			len(got), len(plaintext))
	}
}

func TestEncryptBadKeyLength(t *testing.T) {
	_, err := Encrypt("hello", []byte("short"))
	if err == nil {
		t.Error("Encrypt with short key should fail")
	}
}

func TestDecryptBadKeyLength(t *testing.T) {
	_, err := Decrypt([]byte("some-data-here-long-enough"), []byte("short"))
	if err == nil {
		t.Error("Decrypt with short key should fail")
	}
}

func TestDecryptTooShort(t *testing.T) {
	key := DeriveKey("test-key", testSalt)
	_, err := Decrypt([]byte("short"), key)
	if err == nil {
		t.Error("Decrypt with data shorter than nonce should fail")
	}
}

func TestDeriveKeyV1_Deterministic(t *testing.T) {
	k1 := DeriveKeyV1("legacy-passphrase")
	k2 := DeriveKeyV1("legacy-passphrase")
	if !bytes.Equal(k1, k2) {
		t.Error("DeriveKeyV1 is not deterministic")
	}
}

func TestDeriveKeyV1_Length(t *testing.T) {
	k := DeriveKeyV1("any")
	if len(k) != 32 {
		t.Errorf("DeriveKeyV1 length = %d, want 32", len(k))
	}
}

func TestDeriveKeyV1_DifferentFromCurrent(t *testing.T) {
	v1 := DeriveKeyV1("same-passphrase")
	cur := DeriveKey("same-passphrase", testSalt)
	if bytes.Equal(v1, cur) {
		t.Error("V1 and current keys must differ for the same passphrase")
	}
}

func TestDeriveKeyV1_DifferentPassphrases(t *testing.T) {
	k1 := DeriveKeyV1("passphrase-one")
	k2 := DeriveKeyV1("passphrase-two")
	if bytes.Equal(k1, k2) {
		t.Error("Different passphrases must produce different V1 keys")
	}
}

func TestDeriveKeyV2Legacy_Deterministic(t *testing.T) {
	k1 := DeriveKeyV2Legacy("legacy-passphrase")
	k2 := DeriveKeyV2Legacy("legacy-passphrase")
	if !bytes.Equal(k1, k2) {
		t.Error("DeriveKeyV2Legacy is not deterministic")
	}
}

func TestDeriveKeyV2Legacy_DifferentFromCurrent(t *testing.T) {
	v2 := DeriveKeyV2Legacy("same-passphrase")
	cur := DeriveKey("same-passphrase", testSalt)
	if bytes.Equal(v2, cur) {
		t.Error("V2 legacy and current-salted keys must differ")
	}
}
