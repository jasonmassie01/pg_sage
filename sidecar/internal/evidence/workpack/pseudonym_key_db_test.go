package workpack

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// evid-02c: the pseudonym key is 32 random bytes created on first use and
// stored in sage.workload_pseudonym_key sealed under the encryption
// keyring, so a pseudonym survives a restart and a key rotation.

func pseudonymDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func keyring(t *testing.T, active byte, previous ...byte) *crypto.Keyring {
	t.Helper()
	var prev [][]byte
	for _, p := range previous {
		prev = append(prev, bytes.Repeat([]byte{p}, 32))
	}
	kr, err := crypto.NewKeyring(bytes.Repeat([]byte{active}, 32), prev...)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func sealedPseudonymRow(t *testing.T, pool *pgxpool.Pool) (int, string) {
	t.Helper()
	var n int
	var sealed string
	if err := pool.QueryRow(context.Background(), `SELECT count(*),
		coalesce(max(sealed), '') FROM sage.workload_pseudonym_key`).Scan(&n,
		&sealed); err != nil {
		t.Fatal(err)
	}
	return n, sealed
}

func TestPseudonymKeyDB_CreatedOnceSealedAndStableAcrossRestartAndRotation(t *testing.T) {
	pool := pseudonymDB(t, "workpack_pseudonym_key")
	ctx := context.Background()
	first, err := LoadOrCreatePseudonymKey(ctx, pool, keyring(t, 1))
	if err != nil || len(first) != PseudonymKeyBytes {
		t.Fatalf("create: %v (%d bytes)", err, len(first))
	}
	n, sealed := sealedPseudonymRow(t, pool)
	if n != 1 || !crypto.IsSealed(sealed) || bytes.Contains([]byte(sealed), first) {
		t.Fatalf("row count %d sealed %v", n, crypto.IsSealed(sealed))
	}
	restart, err := LoadOrCreatePseudonymKey(ctx, pool, keyring(t, 1))
	if err != nil || !bytes.Equal(restart, first) {
		t.Fatalf("restart: %v, same key %v", err, bytes.Equal(restart, first))
	}
	// Rotation: key 2 active, key 1 previous; the key is re-sealed under 2.
	rotated, err := LoadOrCreatePseudonymKey(ctx, pool, keyring(t, 2, 1))
	if err != nil || !bytes.Equal(rotated, first) {
		t.Fatalf("rotation: %v, same key %v", err, bytes.Equal(rotated, first))
	}
	afterDrop, err := LoadOrCreatePseudonymKey(ctx, pool, keyring(t, 2))
	if err != nil || !bytes.Equal(afterDrop, first) {
		t.Fatalf("after dropping the old encryption key: %v", err)
	}
	a := hashed(t, modelFixture(t))
	b := mustForModel(t, modelFixture(t), ModelOptions{HashIdentifiers: true,
		PseudonymKey: afterDrop})
	c := mustForModel(t, modelFixture(t), ModelOptions{HashIdentifiers: true,
		PseudonymKey: first})
	if b.ContentSHA256 != c.ContentSHA256 || a.ContentSHA256 == b.ContentSHA256 {
		t.Fatal("the stored key does not give stable pseudonyms")
	}
}

func TestPseudonymKeyDB_NoKeyringCreatesNothing(t *testing.T) {
	pool := pseudonymDB(t, "workpack_pseudonym_nokr")
	key, err := LoadOrCreatePseudonymKey(context.Background(), pool, nil)
	if !errors.Is(err, crypto.ErrNoKeyring) || key != nil {
		t.Fatalf("no keyring: %v", err)
	}
	if n, _ := sealedPseudonymRow(t, pool); n != 0 {
		t.Fatalf("%d rows written without a keyring", n)
	}
	if _, err := LoadOrCreatePseudonymKey(context.Background(), nil,
		keyring(t, 1)); err == nil {
		t.Fatal("no database accepted")
	}
}

func TestPseudonymKeyDB_AnotherEncryptionKeyIsRefusedAndTheRowKept(t *testing.T) {
	pool := pseudonymDB(t, "workpack_pseudonym_wrong")
	ctx := context.Background()
	if _, err := LoadOrCreatePseudonymKey(ctx, pool, keyring(t, 1)); err != nil {
		t.Fatal(err)
	}
	_, before := sealedPseudonymRow(t, pool)
	_, err := LoadOrCreatePseudonymKey(ctx, pool, keyring(t, 9))
	if !errors.Is(err, ErrPseudonymKeyUnreadable) {
		t.Fatalf("wrong encryption key: %v, want ErrPseudonymKeyUnreadable", err)
	}
	if _, after := sealedPseudonymRow(t, pool); after != before {
		t.Fatal("an unreadable pseudonym key was replaced")
	}
}

func TestPseudonymKeyDB_ConcurrentCallersAgree(t *testing.T) {
	pool := pseudonymDB(t, "workpack_pseudonym_race")
	kr := keyring(t, 1)
	keys := make([][]byte, 6)
	errs := make([]error, 6)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = LoadOrCreatePseudonymKey(context.Background(), pool, kr)
		}()
	}
	wg.Wait()
	for i := range keys {
		if errs[i] != nil || !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("caller %d: %v, same key %v", i, errs[i], bytes.Equal(keys[i], keys[0]))
		}
	}
	if n, _ := sealedPseudonymRow(t, pool); n != 1 {
		t.Fatalf("%d rows", n)
	}
}
