package schema

import (
	"context"
	"testing"
)

// TestReadOrCreateKDFSalt_CreatesOnFirstCall verifies the happy path:
// on a fresh sage.crypto_meta table, ReadOrCreateKDFSalt generates and
// persists a random salt, then returns it.
func TestReadOrCreateKDFSalt_CreatesOnFirstCall(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)

	// Clear any existing row so we exercise the insert branch.
	if _, err := pool.Exec(ctx,
		"DELETE FROM sage.crypto_meta WHERE id = 1"); err != nil {
		t.Fatalf("clean crypto_meta: %v", err)
	}

	salt, err := ReadOrCreateKDFSalt(ctx, pool)
	if err != nil {
		t.Fatalf("ReadOrCreateKDFSalt: %v", err)
	}
	if len(salt) < 8 {
		t.Errorf("returned salt length = %d, want >= 8", len(salt))
	}

	// Row must actually have been persisted.
	var persisted []byte
	if err := pool.QueryRow(ctx,
		"SELECT kdf_salt FROM sage.crypto_meta WHERE id = 1").
		Scan(&persisted); err != nil {
		t.Fatalf("verify insert: %v", err)
	}
	if !bytesEqual(persisted, salt) {
		t.Error("persisted salt does not match returned salt")
	}
}

// TestReadOrCreateKDFSalt_ReturnsExisting verifies that a second call
// returns the same salt bytes — stability across restarts is the core
// contract.
func TestReadOrCreateKDFSalt_ReturnsExisting(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)

	first, err := ReadOrCreateKDFSalt(ctx, pool)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := ReadOrCreateKDFSalt(ctx, pool)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !bytesEqual(first, second) {
		t.Errorf("salt changed between calls:\nfirst = %x\nsecond = %x",
			first, second)
	}
}

// TestReadOrCreateKDFSalt_TooShortSaltRejected verifies the short-salt
// guard at crypto_meta.go:33 — if the DB row is somehow corrupted with
// a salt shorter than 8 bytes, we refuse to use it rather than silently
// weakening the KDF.
func TestReadOrCreateKDFSalt_TooShortSaltRejected(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)

	if _, err := pool.Exec(ctx,
		`INSERT INTO sage.crypto_meta (id, kdf_salt)
		 VALUES (1, $1)
		 ON CONFLICT (id) DO UPDATE SET kdf_salt = EXCLUDED.kdf_salt`,
		[]byte{0x01, 0x02, 0x03}, // 3 bytes — below the 8-byte floor
	); err != nil {
		t.Fatalf("seed short salt: %v", err)
	}

	_, err := ReadOrCreateKDFSalt(ctx, pool)
	if err == nil {
		t.Fatal("expected error for too-short salt, got nil")
	}
	if !containsAll(err.Error(), "too short") {
		t.Errorf("error = %q, want it to mention 'too short'", err.Error())
	}

	// Restore a valid salt so subsequent tests/processes are unaffected.
	if _, err := pool.Exec(ctx,
		"DELETE FROM sage.crypto_meta WHERE id = 1"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := ReadOrCreateKDFSalt(ctx, pool); err != nil {
		t.Fatalf("restore salt: %v", err)
	}
}

// TestReadOrCreateKDFSalt_ContextCancelled verifies error propagation
// when the caller's context is already cancelled — the function must
// return an error rather than hang or return a zero-value salt.
func TestReadOrCreateKDFSalt_ContextCancelled(t *testing.T) {
	pool, _ := requireDB(t)
	bootstrapWithRetry(t, context.Background(), pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	salt, err := ReadOrCreateKDFSalt(ctx, pool)
	if err == nil {
		t.Fatalf("expected error for cancelled context, got salt=%x", salt)
	}
	if salt != nil {
		t.Errorf("salt = %x on cancelled ctx, want nil", salt)
	}
}

// ----- small helpers (avoid importing bytes/strings across tests) -----

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
