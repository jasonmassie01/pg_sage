package schema

import (
	"context"
	"sync"
	"testing"
)

// security-08: sage.crypto_meta.generated_key_id records the id of a key
// pg_sage generated, so a lost key file is detected instead of replaced.

func TestGeneratedKeyID_BootstrapTwiceAddsNullableColumn(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var nullable string
	if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'crypto_meta'
		  AND column_name = 'generated_key_id'`).Scan(&nullable); err != nil {
		t.Fatalf("generated_key_id column: %v", err)
	}
	if nullable != "YES" {
		t.Fatalf("generated_key_id is_nullable = %s, want YES", nullable)
	}
}

func TestGeneratedKeyID_NoRowAndNullReadEmpty(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.crypto_meta WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if id, err := ReadGeneratedKeyID(ctx, pool); err != nil || id != "" {
		t.Fatalf("no row: ReadGeneratedKeyID = %q, %v; want empty", id, err)
	}
	if _, err := ReadOrCreateKDFSalt(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if id, err := ReadGeneratedKeyID(ctx, pool); err != nil || id != "" {
		t.Fatalf("null column: ReadGeneratedKeyID = %q, %v; want empty", id, err)
	}
}

func TestGeneratedKeyID_RecordedOnceFirstWriterWins(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.crypto_meta WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOrCreateKDFSalt(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	winners := make([]string, 8)
	errs := make([]error, 8)
	for i := range winners {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			winners[i], errs[i] = RecordGeneratedKeyID(context.Background(), pool,
				string(rune('a'+i))+"000000000000000")
		}(i)
	}
	wg.Wait()
	stored, err := ReadGeneratedKeyID(ctx, pool)
	if err != nil || stored == "" {
		t.Fatalf("stored id = %q, %v", stored, err)
	}
	for i := range winners {
		if errs[i] != nil || winners[i] != stored {
			t.Errorf("writer %d saw %q, %v; want the one stored id %q",
				i, winners[i], errs[i], stored)
		}
	}
	again, err := RecordGeneratedKeyID(ctx, pool, "zzzzzzzzzzzzzzzz")
	if err != nil || again != stored {
		t.Fatalf("later record = %q, %v; want the first id %q kept", again, err, stored)
	}
}

func TestGeneratedKeyID_RecordWithoutSaltRowFails(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.crypto_meta WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordGeneratedKeyID(ctx, pool, "abcdabcdabcdabcd"); err == nil {
		t.Fatal("recording a key id with no crypto_meta row succeeded")
	}
}

// Boundary: the config-04 precheck recognises the migration as one column
// check, so a boot that finds the column skips the ALTER and its lock.
func TestGeneratedKeyID_MigrationIsPrecheckable(t *testing.T) {
	stmts := classifyStep(cryptoMetaKeyIDSQL)
	want := catalogCheck{kind: "column", rel: "sage.crypto_meta", col: "generated_key_id"}
	if len(stmts) != 1 || stmts[0].shape != shapeColumns || len(stmts[0].checks) != 1 ||
		stmts[0].checks[0] != want {
		t.Fatalf("classifyStep(cryptoMetaKeyIDSQL) = %+v, want one %+v column check",
			stmts, want)
	}
}
