package notify

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// No concurrent-access tests: SealSecrets and OpenSecrets copy their input
// and keep no shared state.

func TestOpenSecrets_WrongKeyWrapsSentinelAndNamesKey(t *testing.T) {
	sealed, err := SealSecrets(map[string]string{"bot_token": "tok-123"},
		testKeyring(t, bytes.Repeat([]byte{1}, 32)), 1)
	if err != nil {
		t.Fatalf("SealSecrets: %v", err)
	}
	opened, err := OpenSecrets(sealed, testKeyring(t, bytes.Repeat([]byte{2}, 32)), 1)
	if !errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want ErrWrongEncryptionKey", err)
	}
	if opened != nil {
		t.Errorf("opened = %v on failure, want nil", opened)
	}
	for _, want := range []string{"bot_token", "encryption_key", "re-enter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "tok-123") {
		t.Error("error must not hold the secret")
	}
}

func TestOpenSecrets_RightKeyRoundTrip(t *testing.T) {
	key := testKeyring(t, bytes.Repeat([]byte{3}, 32))
	sealed, err := SealSecrets(map[string]string{
		"token": "t-1", "channel": "#ops",
	}, key, 1)
	if err != nil {
		t.Fatalf("SealSecrets: %v", err)
	}
	if !strings.HasPrefix(sealed["token"], crypto.SealedPrefix) || sealed["channel"] != "#ops" {
		t.Fatalf("sealed = %v", sealed)
	}
	opened, err := OpenSecrets(sealed, key, 1)
	if err != nil || opened["token"] != "t-1" || opened["channel"] != "#ops" {
		t.Fatalf("OpenSecrets = %v, %v", opened, err)
	}
}

func TestOpenSecrets_TruncatedValueIsNotWrongKey(t *testing.T) {
	cfg := map[string]string{
		"api_key": legacySealedPrefix + base64.StdEncoding.EncodeToString([]byte("x")),
	}
	_, err := OpenSecrets(cfg, testKeyring(t, bytes.Repeat([]byte{3}, 32)), 1)
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("err = %v, want too short", err)
	}
	if errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Error("a truncated value is not a wrong key")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("err = %q, want it to name api_key", err)
	}
}

func TestOpenSecrets_NoKeyIsNotWrongKey(t *testing.T) {
	sealed, err := SealSecrets(map[string]string{"secret": "s"},
		testKeyring(t, bytes.Repeat([]byte{4}, 32)), 1)
	if err != nil {
		t.Fatalf("SealSecrets: %v", err)
	}
	_, err = OpenSecrets(sealed, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "no encryption key") {
		t.Fatalf("err = %v, want no encryption key", err)
	}
	if errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Error("a missing key is reported as missing, not wrong")
	}
}

func TestOpenSecrets_EmptyConfig(t *testing.T) {
	opened, err := OpenSecrets(map[string]string{}, nil, 1)
	if err != nil || opened == nil || len(opened) != 0 {
		t.Fatalf("OpenSecrets(empty) = %v, %v", opened, err)
	}
}
