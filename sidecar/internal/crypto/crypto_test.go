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

// DecryptWithMigration: data encrypted under the current salted key
// should decrypt without triggering a re-encrypt.
func TestDecryptWithMigration_CurrentHappyPath(t *testing.T) {
	passphrase := "current-passphrase"
	plaintext := "super-secret"
	key := DeriveKey(passphrase, testSalt)

	ciphertext, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, reEnc, needs, err := DecryptWithMigration(
		ciphertext, passphrase, testSalt)
	if err != nil {
		t.Fatalf("DecryptWithMigration: %v", err)
	}
	if got != plaintext {
		t.Errorf("plaintext: got %q, want %q", got, plaintext)
	}
	if needs {
		t.Error("current ciphertext should not require re-encryption")
	}
	if reEnc != nil {
		t.Error("current ciphertext should return nil reEncrypted buffer")
	}
}

// DecryptWithMigration: legacy v1-encrypted data should fall back to
// the v1 key, decrypt successfully, and return freshly re-encrypted
// ciphertext under the current salted key.
func TestDecryptWithMigration_V1FallbackAndReencrypt(t *testing.T) {
	passphrase := "legacy-passphrase"
	plaintext := "legacy-credential"

	// Encrypt under the legacy (v1) key.
	v1Key := DeriveKeyV1(passphrase)
	legacyCt, err := Encrypt(plaintext, v1Key)
	if err != nil {
		t.Fatalf("legacy Encrypt: %v", err)
	}

	got, reEnc, needs, err := DecryptWithMigration(
		legacyCt, passphrase, testSalt)
	if err != nil {
		t.Fatalf("DecryptWithMigration: %v", err)
	}
	if got != plaintext {
		t.Errorf("plaintext: got %q, want %q", got, plaintext)
	}
	if !needs {
		t.Error("v1 ciphertext should require re-encryption")
	}
	if reEnc == nil {
		t.Fatal("re-encrypted buffer should not be nil for v1 data")
	}
	if bytes.Equal(reEnc, legacyCt) {
		t.Error("re-encrypted ciphertext must differ from legacy ciphertext")
	}

	// Verify the re-encrypted payload decrypts under the current key.
	curKey := DeriveKey(passphrase, testSalt)
	reGot, err := Decrypt(reEnc, curKey)
	if err != nil {
		t.Fatalf("Decrypt re-encrypted: %v", err)
	}
	if reGot != plaintext {
		t.Errorf("re-encrypted plaintext: got %q, want %q", reGot, plaintext)
	}
}

// DecryptWithMigration: legacy v2 (deterministic-salt argon2id) data
// should fall back, decrypt, and return re-encrypted ciphertext under
// the current per-deployment-salt key.
func TestDecryptWithMigration_V2LegacyFallbackAndReencrypt(t *testing.T) {
	passphrase := "v2-legacy-passphrase"
	plaintext := "v2-credential"

	v2Key := DeriveKeyV2Legacy(passphrase)
	legacyCt, err := Encrypt(plaintext, v2Key)
	if err != nil {
		t.Fatalf("v2 legacy Encrypt: %v", err)
	}

	got, reEnc, needs, err := DecryptWithMigration(
		legacyCt, passphrase, testSalt)
	if err != nil {
		t.Fatalf("DecryptWithMigration: %v", err)
	}
	if got != plaintext {
		t.Errorf("plaintext: got %q, want %q", got, plaintext)
	}
	if !needs {
		t.Error("v2 legacy ciphertext should require re-encryption")
	}
	if reEnc == nil {
		t.Fatal("re-encrypted buffer should not be nil for v2 legacy data")
	}
	if bytes.Equal(reEnc, legacyCt) {
		t.Error("re-encrypted ciphertext must differ from v2 legacy")
	}
	curKey := DeriveKey(passphrase, testSalt)
	reGot, err := Decrypt(reEnc, curKey)
	if err != nil {
		t.Fatalf("Decrypt re-encrypted: %v", err)
	}
	if reGot != plaintext {
		t.Errorf("re-encrypted plaintext: got %q, want %q",
			reGot, plaintext)
	}
}

// DecryptWithMigration: when ciphertext decrypts under none of the
// three key paths, the function must return a descriptive error.
func TestDecryptWithMigration_AllKeysFail(t *testing.T) {
	// Encrypt under a totally different passphrase + salt.
	strangerKey := DeriveKey("stranger-passphrase", []byte("other-salt-bytes"))
	ciphertext, err := Encrypt("leaked-secret", strangerKey)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	_, reEnc, needs, err := DecryptWithMigration(
		ciphertext, "wrong-passphrase", testSalt)
	if err == nil {
		t.Fatal("expected error when no key matches")
	}
	if needs {
		t.Error("needsReEncrypt must be false on total failure")
	}
	if reEnc != nil {
		t.Error("reEncrypted must be nil on total failure")
	}
	if !strings.Contains(err.Error(), "v3") ||
		!strings.Contains(err.Error(), "v2") ||
		!strings.Contains(err.Error(), "v1") {
		t.Errorf("error should mention all three keys tried, got: %v", err)
	}
}

// DecryptWithMigration: too-short input must surface the decrypt
// error path cleanly.
func TestDecryptWithMigration_TooShort(t *testing.T) {
	_, reEnc, needs, err := DecryptWithMigration(
		[]byte("x"), "any", testSalt)
	if err == nil {
		t.Fatal("expected error for too-short ciphertext")
	}
	if needs || reEnc != nil {
		t.Error("failure path must not indicate re-encryption")
	}
}
