package crypto

import (
	"errors"
	"strings"
	"testing"
)

// No concurrent-access tests: OpenUnbound only reads the immutable
// keyring, which TestKeyring_ConcurrentSealOpen already covers.

func TestKeyring_OpenUnboundActiveAndPrevious(t *testing.T) {
	kr := mustKeyring(t, testKey(1), testKey(2))
	for name, key := range map[string][]byte{"active": testKey(1), "previous": testKey(2)} {
		raw, err := Encrypt("pw-"+name, key)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		plain, kid, err := kr.OpenUnbound(raw)
		if err != nil {
			t.Fatalf("%s: OpenUnbound: %v", name, err)
		}
		if plain != "pw-"+name {
			t.Errorf("%s: plaintext = %q", name, plain)
		}
		if kid != KeyIDFor(key) {
			t.Errorf("%s: key id = %q, want %q", name, kid, KeyIDFor(key))
		}
	}
}

func TestKeyring_OpenUnboundWrongKey(t *testing.T) {
	kr := mustKeyring(t, testKey(1))
	raw, err := Encrypt("pw", testKey(9))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	plain, kid, err := kr.OpenUnbound(raw)
	if !errors.Is(err, ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want ErrWrongEncryptionKey", err)
	}
	if plain != "" || kid != "" {
		t.Errorf("got %q/%q on failure, want empty", plain, kid)
	}
}

func TestKeyring_OpenUnboundTooShortIsNotWrongKey(t *testing.T) {
	kr := mustKeyring(t, testKey(1))
	for _, raw := range [][]byte{nil, {}, []byte("short")} {
		_, _, err := kr.OpenUnbound(raw)
		if err == nil || !strings.Contains(err.Error(), "too short") {
			t.Fatalf("OpenUnbound(%q) err = %v, want too short", raw, err)
		}
		if errors.Is(err, ErrWrongEncryptionKey) {
			t.Error("a truncated value is not a wrong key")
		}
	}
}

func TestKeyring_OpenUnboundRejectsSealedValues(t *testing.T) {
	// A sage-enc:v1 value carries AAD; opening it unbound would drop the
	// binding, so it must never succeed.
	kr := mustKeyring(t, testKey(1))
	sealed, err := kr.Seal("pw", "sage.databases|1|password")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, _, err := kr.OpenUnbound([]byte(sealed)); err == nil {
		t.Fatal("OpenUnbound opened a sealed value")
	}
}

func TestKeyring_OpenUnboundNilKeyring(t *testing.T) {
	var kr *Keyring
	raw, err := Encrypt("pw", testKey(1))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, _, err = kr.OpenUnbound(raw)
	if !errors.Is(err, ErrNoKeyring) || errors.Is(err, ErrWrongEncryptionKey) {
		t.Fatalf("nil keyring err = %v, want ErrNoKeyring (not a wrong key)", err)
	}
}
