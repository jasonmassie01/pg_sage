package evidence

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// audit-10: with an encryption key, the sidecar keeps one Ed25519
// evidence-signing key in sage.evidence_signing_key, sealed under the
// encryption key, so packs are signed without an operator-supplied key.

func testKeyring(t *testing.T, fill byte, previous ...byte) *crypto.Keyring {
	t.Helper()
	material := func(b byte) []byte {
		k := make([]byte, 32)
		for i := range k {
			k[i] = b
		}
		return k
	}
	var prev [][]byte
	for _, p := range previous {
		prev = append(prev, material(p))
	}
	kr, err := crypto.NewKeyring(material(fill), prev...)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return kr
}

type keyRow struct {
	count  int
	keyID  string
	sealed string
}

func readKeyRow(t *testing.T, pool *pgxpool.Pool) keyRow {
	t.Helper()
	var r keyRow
	err := pool.QueryRow(context.Background(), `SELECT count(*),
		coalesce(max(key_id), ''), coalesce(max(sealed), '')
		FROM sage.evidence_signing_key`).Scan(&r.count, &r.keyID, &r.sealed)
	if err != nil {
		t.Fatalf("read sage.evidence_signing_key: %v", err)
	}
	return r
}

// Happy path: the first call creates one sealed row; the key it returns
// signs a pack that verifies under its public key, and the row never holds
// the private key in the clear.
func TestLoadOrCreateKey_CreatesSealedKeyThatSigns(t *testing.T) {
	pool := freshDB(t, "evidence_key_create")
	ctx := context.Background()
	got, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 1))
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if !got.Created || len(got.Key) != ed25519.PrivateKeySize {
		t.Fatalf("first call: created=%v key len %d", got.Created, len(got.Key))
	}
	pub := got.Key.Public().(ed25519.PublicKey)
	if got.KeyID != KeyID(pub) {
		t.Fatalf("key id %q, want %q", got.KeyID, KeyID(pub))
	}
	row := readKeyRow(t, pool)
	if row.count != 1 || row.keyID != got.KeyID || !crypto.IsSealed(row.sealed) {
		t.Fatalf("row = %+v, want one sealed row with key id %s", row, got.KeyID)
	}
	pemText, _ := MarshalSigningKey(got.Key)
	if row.sealed == pemText || len(row.sealed) < 64 {
		t.Fatalf("the stored key is not sealed: %q", row.sealed)
	}
	pack := build(t, pool, Scope{Database: "orders", ActionID: seedAction(t, pool)}, got.Key)
	if _, err := Verify(bytes.NewReader(pack), pub); err != nil {
		t.Fatalf("a pack signed by the generated key does not verify: %v", err)
	}
}

// State transition: a restart (a new keyring of the same encryption key)
// reads the stored key back; the key id does not change and no row is
// added.
func TestLoadOrCreateKey_RestartReusesTheStoredKey(t *testing.T) {
	pool := freshDB(t, "evidence_key_restart")
	ctx := context.Background()
	first, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 2))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 2))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if second.Created || second.KeyID != first.KeyID || !second.Key.Equal(first.Key) {
		t.Fatalf("restart: created=%v id %s (first %s), same key %v", second.Created,
			second.KeyID, first.KeyID, second.Key.Equal(first.Key))
	}
	if row := readKeyRow(t, pool); row.count != 1 {
		t.Fatalf("%d rows after a restart, want 1", row.count)
	}
}

// Concurrent access: sidecars starting together agree on one row and one
// key; exactly one of them created it.
func TestLoadOrCreateKey_ConcurrentCallersAgree(t *testing.T) {
	pool := freshDB(t, "evidence_key_concurrent")
	const n = 8
	var wg sync.WaitGroup
	results := make([]StoredKey, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = LoadOrCreateKey(context.Background(), pool,
				testKeyring(t, 3))
		}(i)
	}
	wg.Wait()
	created := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i].KeyID != results[0].KeyID || !results[i].Key.Equal(results[0].Key) {
			t.Fatalf("caller %d got key %s, caller 0 got %s", i, results[i].KeyID,
				results[0].KeyID)
		}
		if results[i].Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d callers report creating the key, want 1", created)
	}
	if row := readKeyRow(t, pool); row.count != 1 || row.keyID != results[0].KeyID {
		t.Fatalf("row = %+v, want one row with key %s", row, results[0].KeyID)
	}
}

// Error propagation: a corrupt sealed value is reported as unreadable and
// the row is left as it was: no new key replaces evidence of the old one.
func TestLoadOrCreateKey_CorruptStoredKeyIsNotReplaced(t *testing.T) {
	pool := freshDB(t, "evidence_key_corrupt")
	ctx := context.Background()
	first, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 4))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	corrupt := "sage-enc:v1:" + testKeyring(t, 4).ActiveKeyID() + ":AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	mustExec(t, pool, `UPDATE sage.evidence_signing_key SET sealed = $1`, corrupt)
	got, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 4))
	if !errors.Is(err, ErrStoredKeyUnreadable) || got.Key != nil {
		t.Fatalf("corrupt row: key %v, err %v; want ErrStoredKeyUnreadable", got.Key, err)
	}
	if row := readKeyRow(t, pool); row.count != 1 || row.sealed != corrupt ||
		row.keyID != first.KeyID {
		t.Fatalf("the corrupt row was overwritten: %+v", row)
	}
}

// Error propagation: a different encryption key cannot open the stored
// key, and does not mint a new one either.
func TestLoadOrCreateKey_WrongEncryptionKeyIsUnreadable(t *testing.T) {
	pool := freshDB(t, "evidence_key_wrongkey")
	ctx := context.Background()
	if _, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 5)); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := readKeyRow(t, pool)
	_, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 6))
	if !errors.Is(err, ErrStoredKeyUnreadable) || !errors.Is(err, crypto.ErrUnknownKeyID) {
		t.Fatalf("wrong encryption key: err = %v", err)
	}
	if after := readKeyRow(t, pool); after != before {
		t.Fatalf("row changed under the wrong key: %+v -> %+v", before, after)
	}
}

// Error propagation: a stored key whose recorded key id does not match
// the key is refused (a swapped row).
func TestLoadOrCreateKey_KeyIDMismatchIsUnreadable(t *testing.T) {
	pool := freshDB(t, "evidence_key_idmismatch")
	ctx := context.Background()
	if _, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 7)); err != nil {
		t.Fatalf("create: %v", err)
	}
	mustExec(t, pool, `UPDATE sage.evidence_signing_key SET key_id = '0000000000000000'`)
	if _, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 7)); !errors.Is(err,
		ErrStoredKeyUnreadable) {
		t.Fatalf("mismatched key id: err = %v", err)
	}
}

// State transition: an encryption-key rotation (new active, old previous)
// still opens the key and re-seals it under the new key, so dropping the
// previous key later keeps the same signing key.
func TestLoadOrCreateKey_RotationReseals(t *testing.T) {
	pool := freshDB(t, "evidence_key_rotate")
	ctx := context.Background()
	first, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 8))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rotated, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 9, 8))
	if err != nil || rotated.ResealErr != nil || rotated.KeyID != first.KeyID {
		t.Fatalf("rotation: id %s (want %s), err %v, reseal %v", rotated.KeyID,
			first.KeyID, err, rotated.ResealErr)
	}
	sealedID, err := crypto.SealedKeyID(readKeyRow(t, pool).sealed)
	if err != nil || sealedID != testKeyring(t, 9).ActiveKeyID() {
		t.Fatalf("sealed under %s (%v), want the new key %s", sealedID, err,
			testKeyring(t, 9).ActiveKeyID())
	}
	after, err := LoadOrCreateKey(ctx, pool, testKeyring(t, 9))
	if err != nil || after.KeyID != first.KeyID || after.Created {
		t.Fatalf("after dropping the previous key: id %s err %v created %v",
			after.KeyID, err, after.Created)
	}
}

// Nil/empty: without an encryption key nothing is created and the caller
// learns why.
func TestLoadOrCreateKey_NoKeyringCreatesNothing(t *testing.T) {
	pool := freshDB(t, "evidence_key_nokr")
	got, err := LoadOrCreateKey(context.Background(), pool, nil)
	if !errors.Is(err, crypto.ErrNoKeyring) || got.Key != nil {
		t.Fatalf("nil keyring: key %v err %v, want crypto.ErrNoKeyring", got.Key, err)
	}
	if row := readKeyRow(t, pool); row.count != 0 {
		t.Fatalf("%d rows without an encryption key, want 0", row.count)
	}
	if _, err := LoadOrCreateKey(context.Background(), nil, testKeyring(t, 1)); err == nil {
		t.Fatal("a nil database was accepted")
	}
}

// Error propagation: a database without the table (the migration did not
// run) fails with an error naming the table.
func TestLoadOrCreateKey_MissingTableNamesIt(t *testing.T) {
	pool := freshDB(t, "evidence_key_notable")
	mustExec(t, pool, `DROP TABLE sage.evidence_signing_key`)
	_, err := LoadOrCreateKey(context.Background(), pool, testKeyring(t, 1))
	if err == nil || !strings.Contains(err.Error(), "sage.evidence_signing_key") {
		t.Fatalf("missing table: err = %v", err)
	}
}
