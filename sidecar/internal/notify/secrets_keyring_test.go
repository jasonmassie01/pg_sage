package notify

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// security-08: channel secrets are sage-enc:v1 values bound to their
// channel and key name. No concurrent-access tests: these functions copy
// their input and the keyring is immutable.

func testKeyring(t *testing.T, active []byte, previous ...[]byte) *crypto.Keyring {
	t.Helper()
	kr, err := crypto.NewKeyring(active, previous...)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

func legacyEncV1(t *testing.T, plain string, key []byte) string {
	t.Helper()
	ct, err := crypto.Encrypt(plain, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return legacySealedPrefix + base64.StdEncoding.EncodeToString(ct)
}

func TestChannelSecretAAD(t *testing.T) {
	if got := ChannelSecretAAD(12, "bot_token"); got != "sage.notification_channels|12|bot_token" {
		t.Fatalf("ChannelSecretAAD = %q", got)
	}
}

func TestSealSecrets_SealedValueNamesActiveKey(t *testing.T) {
	kr := testKeyring(t, bytes.Repeat([]byte{1}, 32))
	sealed, err := SealSecrets(map[string]string{"routing_key": "rk-1", "x": "y"}, kr, 3)
	if err != nil {
		t.Fatalf("SealSecrets: %v", err)
	}
	kid, err := crypto.SealedKeyID(sealed["routing_key"])
	if err != nil || kid != kr.ActiveKeyID() {
		t.Fatalf("routing_key = %q (kid %q, %v)", sealed["routing_key"], kid, err)
	}
	if sealed["x"] != "y" {
		t.Errorf("non-secret changed: %q", sealed["x"])
	}
}

func TestOpenSecrets_SealedValueMovedToAnotherChannelOrKeyFails(t *testing.T) {
	kr := testKeyring(t, bytes.Repeat([]byte{1}, 32))
	sealed, err := SealSecrets(map[string]string{"token": "t-1"}, kr, 1)
	if err != nil {
		t.Fatalf("SealSecrets: %v", err)
	}
	if _, err := OpenSecrets(sealed, kr, 2); !errors.Is(err, crypto.ErrOpenFailed) {
		t.Fatalf("other channel: err = %v, want ErrOpenFailed", err)
	}
	moved := map[string]string{"secret": sealed["token"]}
	if _, err := OpenSecrets(moved, kr, 1); !errors.Is(err, crypto.ErrOpenFailed) {
		t.Fatalf("other key name: err = %v, want ErrOpenFailed", err)
	}
}

func TestOpenSecrets_LegacyEncV1UnderActiveOrPrevious(t *testing.T) {
	keyA, keyB := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	kr := testKeyring(t, keyB, keyA)
	cfg := map[string]string{
		"webhook_url": legacyEncV1(t, "https://hooks.example.com/a", keyA),
		"api_key":     legacyEncV1(t, "k-b", keyB),
		"smtp_pass":   "plain-legacy",
	}
	opened, err := OpenSecrets(cfg, kr, 7)
	if err != nil {
		t.Fatalf("OpenSecrets: %v", err)
	}
	if opened["webhook_url"] != "https://hooks.example.com/a" ||
		opened["api_key"] != "k-b" || opened["smtp_pass"] != "plain-legacy" {
		t.Fatalf("opened = %v", opened)
	}
}

func TestOpenSecrets_LegacyEncV1UnderUnknownKeyIsWrongKey(t *testing.T) {
	kr := testKeyring(t, bytes.Repeat([]byte{1}, 32))
	cfg := map[string]string{"api_key": legacyEncV1(t, "k", bytes.Repeat([]byte{9}, 32))}
	_, err := OpenSecrets(cfg, kr, 1)
	if !errors.Is(err, crypto.ErrWrongEncryptionKey) || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("err = %v, want ErrWrongEncryptionKey naming api_key", err)
	}
}

func TestNeedsReseal(t *testing.T) {
	keyA, keyB := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	krA, krB := testKeyring(t, keyA), testKeyring(t, keyB, keyA)
	underA, err := SealSecrets(map[string]string{"token": "t"}, krA, 1)
	if err != nil {
		t.Fatal(err)
	}
	underB, err := SealSecrets(map[string]string{"token": "t"}, krB, 1)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cfg  map[string]string
		kr   *crypto.Keyring
		want bool
	}{
		{"plaintext with key", map[string]string{"token": "t"}, krB, true},
		{"plaintext without key", map[string]string{"token": "t"}, nil, false},
		{"empty secret", map[string]string{"token": ""}, krB, false},
		{"non-secret only", map[string]string{"channel": "#ops"}, krB, false},
		{"enc:v1", map[string]string{"token": legacyEncV1(t, "t", keyA)}, krB, true},
		{"sealed under previous", underA, krB, true},
		{"sealed under active", underB, krB, false},
		{"nil config", nil, krB, false},
	}
	for _, tc := range cases {
		if got := NeedsReseal(tc.cfg, tc.kr); got != tc.want {
			t.Errorf("%s: NeedsReseal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSealSecrets_NilKeyringKeepsPlaintext(t *testing.T) {
	out, err := SealSecrets(map[string]string{"token": "t"}, nil, 1)
	if err != nil || out["token"] != "t" {
		t.Fatalf("SealSecrets(nil keyring) = %v, %v", out, err)
	}
}
