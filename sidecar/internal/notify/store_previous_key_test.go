package notify

import (
	"bytes"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// security-08 review: the runtime PoolStore must open channel secrets
// sealed under the previous key of a rotation, or deliveries fail until
// someone re-enters every secret (YAML fleet has no startup re-seal). No
// concurrent-access tests: WithPreviousKey runs once at construction and
// the keyring it builds is immutable.

func TestPoolStoreWithPreviousKeyOpensPreviousSeal(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte{5}, 32), bytes.Repeat([]byte{6}, 32)
	sealed, err := SealSecrets(map[string]string{"bot_token": "tok-prev"},
		testKeyring(t, oldKey), 9)
	if err != nil {
		t.Fatalf("SealSecrets: %v", err)
	}
	activeOnly := NewPoolStore(nil, newKey)
	if _, err := OpenSecrets(sealed, activeOnly.keyring, 9); !errors.Is(err,
		crypto.ErrWrongEncryptionKey) {
		t.Fatalf("active-only open err = %v, want ErrWrongEncryptionKey", err)
	}
	s := NewPoolStore(nil, newKey).WithPreviousKey(oldKey)
	opened, err := OpenSecrets(sealed, s.keyring, 9)
	if err != nil || opened["bot_token"] != "tok-prev" {
		t.Fatalf("OpenSecrets = %v, %v; want tok-prev", opened, err)
	}
	if got, want := s.keyring.ActiveKeyID(), crypto.KeyIDFor(newKey); got != want {
		t.Fatalf("active key id = %s, want the new key %s", got, want)
	}
}

func TestPoolStoreWithPreviousKeyEmptyAndNoActive(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	s := NewPoolStore(nil, key).WithPreviousKey(nil)
	if s.keyring == nil || s.keyring.ActiveKeyID() != crypto.KeyIDFor(key) {
		t.Fatalf("empty previous key dropped the active key: %+v", s.keyring)
	}
	// Without an active key nothing is sealed or opened, previous or not.
	if s := NewPoolStore(nil, nil).WithPreviousKey(key); s.keyring != nil {
		t.Fatal("a previous key without an active key built a keyring")
	}
}
