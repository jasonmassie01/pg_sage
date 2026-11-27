package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// These tests use a nil pool: a wrong key or a current-key hit never reaches
// the database, and a nil pool would panic if one tried to.

var (
	unitSalt = []byte("unit-store-salt1")
	// keyA sealed the stored values; keyB is the operator's new key.
	unitKeyA = crypto.DeriveKey("passphrase-a", unitSalt)
	unitKeyB = crypto.DeriveKey("passphrase-b", unitSalt)
)

func sealed(t testing.TB, plaintext string, key []byte) []byte {
	t.Helper()
	ct, err := crypto.Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return ct
}

// captureSlog sends slog output to a buffer for the duration of the test.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWithKeyMigration_DerivesLegacyKeysOnce(t *testing.T) {
	before := crypto.Argon2Derivations()
	s := NewDatabaseStore(nil, unitKeyB).WithKeyMigration("passphrase-b")
	if got := crypto.Argon2Derivations() - before; got != 1 {
		t.Errorf("argon2 derivations at construction = %d, want 1", got)
	}
	ct := sealed(t, "pw", unitKeyB)
	before = crypto.Argon2Derivations()
	for i := 0; i < 5; i++ {
		if pw, err := s.decryptPassword(context.Background(), 1, ct); err != nil || pw != "pw" {
			t.Fatalf("decryptPassword = %q, %v", pw, err)
		}
	}
	if got := crypto.Argon2Derivations() - before; got != 0 {
		t.Errorf("argon2 derivations on reads = %d, want 0", got)
	}
}

func TestDecryptPassword_WrongKeyNamesDatabase(t *testing.T) {
	captureSlog(t)
	s := NewDatabaseStore(nil, unitKeyB).WithKeyMigration("passphrase-b")
	pw, err := s.decryptPassword(context.Background(), 7, sealed(t, "pw", unitKeyA))
	if !errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want ErrWrongEncryptionKey", err)
	}
	if !strings.Contains(err.Error(), "database 7") {
		t.Errorf("err = %q, want it to name database 7", err)
	}
	if pw != "" {
		t.Errorf("password = %q on failure, want empty", pw)
	}
}

func TestDecryptPassword_WrongKeyDerivesNothing(t *testing.T) {
	captureSlog(t)
	s := NewDatabaseStore(nil, unitKeyB).WithKeyMigration("passphrase-b")
	ct := sealed(t, "pw", unitKeyA)
	before := crypto.Argon2Derivations()
	for id := 1; id <= 10; id++ {
		if _, err := s.decryptPassword(context.Background(), id, ct); err == nil {
			t.Fatal("a value sealed under another key opened")
		}
	}
	if got := crypto.Argon2Derivations() - before; got != 0 {
		t.Errorf("argon2 derivations for 10 wrong-key reads = %d, want 0", got)
	}
}

func TestDecryptPassword_WrongKeyLogsOncePerDatabase(t *testing.T) {
	logs := captureSlog(t)
	s := NewDatabaseStore(nil, unitKeyB).WithKeyMigration("passphrase-b")
	ct := sealed(t, "hunter2-plaintext", unitKeyA)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _ = s.decryptPassword(ctx, 1, ct)
		_, _ = s.decryptPassword(ctx, 2, ct)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.decryptPassword(ctx, 3, ct)
		}()
	}
	wg.Wait()
	out := logs.String()
	if n := strings.Count(out, "level=ERROR"); n != 3 {
		t.Fatalf("ERROR lines = %d, want 3 (one per database):\n%s", n, out)
	}
	for _, want := range []string{
		"database_id=1", "database_id=2", "database_id=3",
		"was encryption_key changed?", "Re-enter it or restore the old key",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Error("log must never hold the plaintext")
	}
}

func TestDecryptPassword_NoPassphraseKeepsCurrentKeyOnly(t *testing.T) {
	captureSlog(t)
	ctx := context.Background()
	v1 := sealed(t, "legacy", crypto.DeriveKeyV1("passphrase-b"))
	for name, s := range map[string]*DatabaseStore{
		"no migration":     NewDatabaseStore(nil, unitKeyB),
		"empty passphrase": NewDatabaseStore(nil, unitKeyB).WithKeyMigration(""),
	} {
		t.Run(name, func(t *testing.T) {
			if pw, err := s.decryptPassword(ctx, 1, sealed(t, "cur", unitKeyB)); err != nil ||
				pw != "cur" {
				t.Fatalf("current-key read = %q, %v", pw, err)
			}
			// A legacy value is not tried against legacy keys (and the nil
			// pool proves no re-seal was attempted).
			_, err := s.decryptPassword(ctx, 2, v1)
			if !errors.Is(err, crypto.ErrWrongEncryptionKey) {
				t.Fatalf("legacy read err = %v, want ErrWrongEncryptionKey", err)
			}
		})
	}
}

func TestDecryptPassword_EmptyCiphertextIsTooShort(t *testing.T) {
	logs := captureSlog(t)
	s := NewDatabaseStore(nil, unitKeyB).WithKeyMigration("passphrase-b")
	_, err := s.decryptPassword(context.Background(), 4, nil)
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("err = %v, want too short", err)
	}
	if errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Error("an empty value is not a wrong key")
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Error("a truncated value must not log the wrong-key hint")
	}
}

// BenchmarkDecryptPasswordWrongKey reports argon2 derivations per read of a
// password sealed under another key. At 61152ef3 it was 2 per op; now 0.
func BenchmarkDecryptPasswordWrongKey(b *testing.B) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&syncBuffer{}, nil)))
	b.Cleanup(func() { slog.SetDefault(prev) })
	s := NewDatabaseStore(nil, unitKeyB).WithKeyMigration("passphrase-b")
	ct := sealed(b, "pw", unitKeyA)
	ctx := context.Background()
	before := crypto.Argon2Derivations()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.decryptPassword(ctx, i, ct); err == nil {
			b.Fatal("wrong key opened the value")
		}
	}
	b.StopTimer()
	perOp := float64(crypto.Argon2Derivations()-before) / float64(b.N)
	b.ReportMetric(perOp, "argon2/op")
	if perOp != 0 {
		b.Fatalf("argon2 derivations per op = %v, want 0", perOp)
	}
}
