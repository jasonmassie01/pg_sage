package api

import (
	"bytes"
	"testing"
)

// security-08 review: the notification routes and their dispatcher read
// with the same active and previous keys as the runtime dispatchers. No
// concurrent-access test: newNotificationRouteDeps copies values once at
// router construction.

func TestNewNotificationRouteDepsCarriesBothKeys(t *testing.T) {
	active, previous := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	d := newNotificationRouteDeps(RuntimeDeps{
		NotificationSecretKey:         active,
		NotificationPreviousSecretKey: previous,
	})
	if !bytes.Equal(d.secretKey, active) {
		t.Fatalf("secretKey = %x, want the active key", d.secretKey)
	}
	if !bytes.Equal(d.previousKey, previous) {
		t.Fatalf("previousKey = %x, want the previous key", d.previousKey)
	}
}

func TestNewNotificationRouteDepsWithoutKeys(t *testing.T) {
	d := newNotificationRouteDeps(RuntimeDeps{})
	if d.secretKey != nil || d.previousKey != nil {
		t.Fatalf("deps = %+v, want no keys (secrets stay plaintext)", d)
	}
}
