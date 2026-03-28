package crypto

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	key := DeriveKey("test-passphrase")
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
	key := DeriveKey("test-passphrase")
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
	key1 := DeriveKey("correct-key")
	key2 := DeriveKey("wrong-key")

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
	key := DeriveKey("test-key")

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
	k1 := DeriveKey("my-passphrase")
	k2 := DeriveKey("my-passphrase")

	if !bytes.Equal(k1, k2) {
		t.Error("DeriveKey is not deterministic")
	}
}

func TestDeriveKeyLength(t *testing.T) {
	key := DeriveKey("any-passphrase")
	if len(key) != 32 {
		t.Errorf("DeriveKey length = %d, want 32", len(key))
	}
}

func TestEmptyPlaintext(t *testing.T) {
	key := DeriveKey("test-key")

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
	key := DeriveKey("test-key")
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
	key := DeriveKey("test-key")
	_, err := Decrypt([]byte("short"), key)
	if err == nil {
		t.Error("Decrypt with data shorter than nonce should fail")
	}
}
