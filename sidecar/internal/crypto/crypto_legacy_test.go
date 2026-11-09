package crypto

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

const legacyPassphrase = "legacy-passphrase"

func mustEncrypt(t *testing.T, plaintext string, key []byte) []byte {
	t.Helper()
	ct, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return ct
}

func TestDeriveLegacyKeys_MatchesLegacyDerivations(t *testing.T) {
	before := Argon2Derivations()
	keys := DeriveLegacyKeys(legacyPassphrase)
	if got := Argon2Derivations() - before; got != 1 {
		t.Errorf("argon2 derivations = %d, want 1 (v2 only)", got)
	}
	if !bytes.Equal(keys.V2, DeriveKeyV2Legacy(legacyPassphrase)) {
		t.Error("V2 does not match DeriveKeyV2Legacy")
	}
	if !bytes.Equal(keys.V1, DeriveKeyV1(legacyPassphrase)) {
		t.Error("V1 does not match DeriveKeyV1")
	}
}

func TestDeriveLegacyKeys_EmptyPassphraseIsZero(t *testing.T) {
	before := Argon2Derivations()
	keys := DeriveLegacyKeys("")
	if keys.V1 != nil || keys.V2 != nil {
		t.Errorf("empty passphrase gave keys %+v, want zero", keys)
	}
	if !keys.IsZero() {
		t.Error("IsZero = false for empty passphrase")
	}
	if got := Argon2Derivations() - before; got != 0 {
		t.Errorf("argon2 derivations = %d, want 0", got)
	}
}

func TestArgon2Derivations_CountsEachDerivation(t *testing.T) {
	before := Argon2Derivations()
	DeriveKey("p", testSalt)
	DeriveKeyV2Legacy("p")
	DeriveKeyV1("p")
	if got := Argon2Derivations() - before; got != 2 {
		t.Errorf("argon2 derivations = %d, want 2", got)
	}
}

func TestDecryptLegacy_CurrentOpensWithoutReseal(t *testing.T) {
	current := DeriveKey(legacyPassphrase, testSalt)
	ct := mustEncrypt(t, "current-secret", current)
	got, resealed, err := DecryptLegacy(ct, current, DeriveLegacyKeys(legacyPassphrase))
	if err != nil {
		t.Fatalf("DecryptLegacy: %v", err)
	}
	if got != "current-secret" {
		t.Errorf("plaintext = %q, want current-secret", got)
	}
	if resealed != nil {
		t.Error("current-key value must not be re-sealed")
	}
}

func TestDecryptLegacy_LegacyOpensAndReseals(t *testing.T) {
	current := DeriveKey(legacyPassphrase, testSalt)
	legacy := DeriveLegacyKeys(legacyPassphrase)
	for name, key := range map[string][]byte{"v1": legacy.V1, "v2": legacy.V2} {
		t.Run(name, func(t *testing.T) {
			ct := mustEncrypt(t, name+"-secret", key)
			before := Argon2Derivations()
			got, resealed, err := DecryptLegacy(ct, current, legacy)
			if err != nil {
				t.Fatalf("DecryptLegacy: %v", err)
			}
			if d := Argon2Derivations() - before; d != 0 {
				t.Errorf("argon2 derivations = %d, want 0", d)
			}
			if got != name+"-secret" {
				t.Errorf("plaintext = %q", got)
			}
			if resealed == nil || bytes.Equal(resealed, ct) {
				t.Fatal("legacy value must come back re-sealed")
			}
			reopened, err := Decrypt(resealed, current)
			if err != nil || reopened != name+"-secret" {
				t.Errorf("re-sealed value under current key = %q, %v", reopened, err)
			}
		})
	}
}

func TestDecryptLegacy_WrongKeyIsSentinel(t *testing.T) {
	stranger := DeriveKey("stranger", []byte("other-salt-bytes"))
	ct := mustEncrypt(t, "leaked", stranger)
	current := DeriveKey(legacyPassphrase, testSalt)
	before := Argon2Derivations()
	got, resealed, err := DecryptLegacy(ct, current, DeriveLegacyKeys(legacyPassphrase))
	derived := Argon2Derivations() - before
	if !errors.Is(err, ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want ErrWrongEncryptionKey", err)
	}
	if got != "" || resealed != nil {
		t.Errorf("failure returned %q, %v; want zero values", got, resealed)
	}
	// One derivation is DeriveLegacyKeys in the call expression above.
	if derived != 1 {
		t.Errorf("argon2 derivations = %d, want 1 (legacy keys only)", derived)
	}
}

func TestDecryptLegacy_ZeroLegacyKeysTriesCurrentOnly(t *testing.T) {
	current := DeriveKey(legacyPassphrase, testSalt)
	ct := mustEncrypt(t, "v1-secret", DeriveKeyV1(legacyPassphrase))
	_, resealed, err := DecryptLegacy(ct, current, LegacyKeys{})
	if !errors.Is(err, ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want ErrWrongEncryptionKey", err)
	}
	if resealed != nil {
		t.Error("failure must not re-seal")
	}
}

func TestDecryptLegacy_EmptyCiphertextIsTooShort(t *testing.T) {
	current := DeriveKey(legacyPassphrase, testSalt)
	legacy := DeriveLegacyKeys(legacyPassphrase)
	for name, ct := range map[string][]byte{"nil": nil, "empty": {}, "short": []byte("x")} {
		t.Run(name, func(t *testing.T) {
			_, resealed, err := DecryptLegacy(ct, current, legacy)
			if err == nil || !strings.Contains(err.Error(), "too short") {
				t.Fatalf("err = %v, want a too-short error", err)
			}
			if errors.Is(err, ErrWrongEncryptionKey) {
				t.Error("a truncated value is not a wrong key")
			}
			if resealed != nil {
				t.Error("failure must not re-seal")
			}
		})
	}
}

func TestDecryptLegacy_InvalidCurrentKeyLength(t *testing.T) {
	_, _, err := DecryptLegacy(make([]byte, 40), []byte("short"), LegacyKeys{})
	if err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("err = %v, want a key-length error", err)
	}
	if errors.Is(err, ErrWrongEncryptionKey) {
		t.Error("a malformed key is a configuration error, not a wrong key")
	}
}

func TestDecrypt_WrongKeyWrapsSentinel(t *testing.T) {
	ct := mustEncrypt(t, "s", bytes.Repeat([]byte{1}, 32))
	_, err := Decrypt(ct, bytes.Repeat([]byte{2}, 32))
	if !errors.Is(err, ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want ErrWrongEncryptionKey", err)
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("err = %q, want the decrypt prefix", err)
	}
}

func TestDecrypt_TooShortIsNotSentinel(t *testing.T) {
	_, err := Decrypt([]byte{}, bytes.Repeat([]byte{1}, 32))
	if err == nil || errors.Is(err, ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want too short and not the sentinel", err)
	}
}

func TestDecryptLegacy_ConcurrentReadersShareKeys(t *testing.T) {
	current := DeriveKey(legacyPassphrase, testSalt)
	legacy := DeriveLegacyKeys(legacyPassphrase)
	ct := mustEncrypt(t, "shared", legacy.V2)
	var wg sync.WaitGroup
	results := make([]string, 20)
	errs := make([]error, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _, errs[i] = DecryptLegacy(ct, current, legacy)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != "shared" {
			t.Errorf("reader %d = %q, %v", i, results[i], errs[i])
		}
	}
}
