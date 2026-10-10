package crypto

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func mustKeyring(t *testing.T, active []byte, previous ...[]byte) *Keyring {
	t.Helper()
	kr, err := NewKeyring(active, previous...)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

func TestKeyring_SealOpenRoundTrip(t *testing.T) {
	kr := mustKeyring(t, testKey(1))
	sealed, err := kr.Seal("sk-test-plaintext", "sage.config|llm.api_key|0")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, SealedPrefix) {
		t.Fatalf("sealed value %q lacks prefix %q", sealed, SealedPrefix)
	}
	if strings.Contains(sealed, "sk-test-plaintext") {
		t.Fatal("sealed value contains the plaintext")
	}
	if !strings.Contains(sealed, ":"+kr.ActiveKeyID()+":") {
		t.Fatalf("sealed value %q does not carry key id %s", sealed, kr.ActiveKeyID())
	}
	plain, kid, err := kr.Open(sealed, "sage.config|llm.api_key|0")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if plain != "sk-test-plaintext" || kid != kr.ActiveKeyID() {
		t.Fatalf("Open = (%q, %q), want plaintext and active kid %q",
			plain, kid, kr.ActiveKeyID())
	}
}

func TestKeyring_SealUsesFreshNonce(t *testing.T) {
	kr := mustKeyring(t, testKey(1))
	a, _ := kr.Seal("same", "aad")
	b, _ := kr.Seal("same", "aad")
	if a == b {
		t.Fatal("two seals of the same plaintext are identical: nonce reused")
	}
}

func TestKeyring_EmptyPlaintextRoundTrips(t *testing.T) {
	kr := mustKeyring(t, testKey(1))
	sealed, err := kr.Seal("", "aad")
	if err != nil {
		t.Fatalf("Seal empty: %v", err)
	}
	plain, _, err := kr.Open(sealed, "aad")
	if err != nil || plain != "" {
		t.Fatalf("Open empty = (%q, %v), want empty and nil", plain, err)
	}
}

func TestKeyring_WrongAADFails(t *testing.T) {
	kr := mustKeyring(t, testKey(1))
	sealed, _ := kr.Seal("secret", "sage.config|llm.api_key|0")
	_, _, err := kr.Open(sealed, "sage.config|clone.dle_token|0")
	if !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("Open with swapped AAD: err = %v, want ErrOpenFailed", err)
	}
}

func TestKeyring_TamperedCiphertextFails(t *testing.T) {
	kr := mustKeyring(t, testKey(1))
	sealed, _ := kr.Seal("secret", "aad")
	last := sealed[len(sealed)-1]
	flip := byte('A')
	if last == 'A' {
		flip = 'B'
	}
	tampered := sealed[:len(sealed)-1] + string(flip)
	if _, _, err := kr.Open(tampered, "aad"); err == nil {
		t.Fatal("tampered ciphertext opened without error")
	}
}

func TestKeyring_UnknownKeyID(t *testing.T) {
	old := mustKeyring(t, testKey(1))
	sealed, _ := old.Seal("secret", "aad")
	other := mustKeyring(t, testKey(2))
	_, _, err := other.Open(sealed, "aad")
	if !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("Open under unrelated key: err = %v, want ErrUnknownKeyID", err)
	}
}

func TestKeyring_RotationOpensPreviousKey(t *testing.T) {
	old := mustKeyring(t, testKey(1))
	sealed, _ := old.Seal("rotated-secret", "aad")
	rotated := mustKeyring(t, testKey(2), testKey(1))
	if rotated.ActiveKeyID() == old.ActiveKeyID() {
		t.Fatal("new active key has the old key id")
	}
	plain, kid, err := rotated.Open(sealed, "aad")
	if err != nil {
		t.Fatalf("Open under previous key: %v", err)
	}
	if plain != "rotated-secret" || kid != old.ActiveKeyID() {
		t.Fatalf("Open = (%q, %q), want old plaintext and old kid", plain, kid)
	}
	resealed, _ := rotated.Seal(plain, "aad")
	if got, _ := SealedKeyID(resealed); got != rotated.ActiveKeyID() {
		t.Fatalf("re-seal kid = %q, want active %q", got, rotated.ActiveKeyID())
	}
}

func TestNewKeyring_RejectsBadKeys(t *testing.T) {
	cases := map[string]struct {
		active   []byte
		previous [][]byte
	}{
		"nil active":     {nil, nil},
		"empty active":   {[]byte{}, nil},
		"short active":   {testKey(1)[:16], nil},
		"long active":    {append(testKey(1), 0), nil},
		"short previous": {testKey(1), [][]byte{testKey(2)[:31]}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			kr, err := NewKeyring(tc.active, tc.previous...)
			if err == nil || kr != nil {
				t.Fatalf("NewKeyring = (%v, %v), want nil and an error", kr, err)
			}
			if !strings.Contains(err.Error(), "32 bytes") {
				t.Fatalf("error %q does not name the required length", err)
			}
		})
	}
}

func TestNewKeyring_PreviousEqualToActiveIsHarmless(t *testing.T) {
	kr := mustKeyring(t, testKey(1), testKey(1), nil)
	sealed, _ := kr.Seal("x", "aad")
	if plain, _, err := kr.Open(sealed, "aad"); err != nil || plain != "x" {
		t.Fatalf("Open = (%q, %v)", plain, err)
	}
}

func TestKeyIDFor_DeterministicAndDistinct(t *testing.T) {
	a1, a2, b := KeyIDFor(testKey(1)), KeyIDFor(testKey(1)), KeyIDFor(testKey(2))
	if a1 != a2 {
		t.Fatalf("KeyIDFor not deterministic: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatal("different keys share a key id")
	}
	if len(a1) != 16 || strings.Trim(a1, "0123456789abcdef") != "" {
		t.Fatalf("key id %q is not 16 lower-case hex chars", a1)
	}
	if strings.Contains(string(testKey(1)), a1) {
		t.Fatal("key id leaks key material")
	}
}

func TestKeyring_MalformedSealedValues(t *testing.T) {
	kr := mustKeyring(t, testKey(1))
	kid := kr.ActiveKeyID()
	cases := map[string]struct {
		value string
		want  error
	}{
		"plaintext":       {"sk-plain", ErrNotSealed},
		"empty":           {"", ErrNotSealed},
		"prefix only":     {SealedPrefix, ErrMalformedSealed},
		"no payload":      {SealedPrefix + kid, ErrMalformedSealed},
		"empty kid":       {SealedPrefix + ":AAAA", ErrMalformedSealed},
		"bad base64":      {SealedPrefix + kid + ":!!!", ErrMalformedSealed},
		"short payload":   {SealedPrefix + kid + ":AAAA", ErrMalformedSealed},
		"wrong version":   {"sage-enc:v9:" + kid + ":AAAA", ErrNotSealed},
		"case mismatched": {strings.ToUpper(SealedPrefix) + kid + ":AAAA", ErrNotSealed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plain, _, err := kr.Open(tc.value, "aad")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open(%q) err = %v, want %v", tc.value, err, tc.want)
			}
			if plain != "" {
				t.Fatalf("Open(%q) returned plaintext %q on error", tc.value, plain)
			}
		})
	}
}

func TestIsSealedAndSealedKeyID(t *testing.T) {
	kr := mustKeyring(t, testKey(3))
	sealed, _ := kr.Seal("v", "aad")
	if !IsSealed(sealed) {
		t.Fatal("IsSealed(sealed) = false")
	}
	for _, v := range []string{"", "plain", "sage-enc:v2:x:y", " " + sealed} {
		if IsSealed(v) {
			t.Fatalf("IsSealed(%q) = true", v)
		}
	}
	kid, err := SealedKeyID(sealed)
	if err != nil || kid != kr.ActiveKeyID() {
		t.Fatalf("SealedKeyID = (%q, %v), want %q", kid, err, kr.ActiveKeyID())
	}
	if _, err := SealedKeyID("plain"); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("SealedKeyID(plain) err = %v, want ErrNotSealed", err)
	}
}

func TestKeyring_NilReceiver(t *testing.T) {
	var kr *Keyring
	if _, err := kr.Seal("x", "aad"); err == nil {
		t.Fatal("nil keyring sealed a value")
	}
	if _, _, err := kr.Open(SealedPrefix+"abc:AAAA", "aad"); err == nil {
		t.Fatal("nil keyring opened a value")
	}
	if kr.ActiveKeyID() != "" {
		t.Fatal("nil keyring reports an active key id")
	}
}

func TestKeyring_ConcurrentSealOpen(t *testing.T) {
	kr := mustKeyring(t, testKey(4), testKey(5))
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := strings.Repeat("s", i)
			sealed, err := kr.Seal(want, "aad")
			if err != nil {
				errs <- err
				return
			}
			got, _, err := kr.Open(sealed, "aad")
			if err != nil || got != want {
				errs <- errors.New("concurrent round trip mismatch")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
