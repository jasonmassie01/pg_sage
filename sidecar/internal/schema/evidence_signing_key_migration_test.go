package schema

import (
	"context"
	"strings"
	"testing"
)

// audit-10: sage.evidence_signing_key holds the one sealed key the sidecar
// signs evidence packs with when no signing_key_env is configured.

// No concurrent access tests here: two creators are covered by
// evidence.TestLoadOrCreateKey_ConcurrentCallersAgree on the real table.

// Integration: two bootstraps leave one table of the expected shape; it
// holds at most one row (id = 1) and refuses empty values.
func TestEvidenceSigningKeyMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var cols string
	if err := pool.QueryRow(ctx, `SELECT string_agg(attname || ':' ||
		format_type(atttypid, atttypmod) || ':' || attnotnull, ',' ORDER BY attnum)
		FROM pg_attribute WHERE attrelid = 'sage.evidence_signing_key'::regclass
		AND attnum > 0 AND NOT attisdropped`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	want := "id:integer:true,key_id:text:true,sealed:text:true," +
		"created_at:timestamp with time zone:true"
	if cols != want {
		t.Fatalf("columns = %s, want %s", cols, want)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.evidence_signing_key`)
	})
	if _, err := pool.Exec(ctx, `DELETE FROM sage.evidence_signing_key`); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO sage.evidence_signing_key (id, key_id, sealed) VALUES ($1, $2, $3)`
	if _, err := pool.Exec(ctx, ins, 1, "0123456789abcdef", "sage-enc:v1:k:x"); err != nil {
		t.Fatalf("the singleton row: %v", err)
	}
	for _, bad := range [][3]any{{2, "0123456789abcdef", "sage-enc:v1:k:x"},
		{1, "0123456789abcdef", "sage-enc:v1:k:y"}} {
		if _, err := pool.Exec(ctx, ins, bad[:]...); err == nil {
			t.Fatalf("row %v was accepted beside the singleton", bad)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.evidence_signing_key`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][3]any{{1, "", "sage-enc:v1:k:x"}, {1, "0123456789abcdef", ""}} {
		_, err := pool.Exec(ctx, ins, bad[:]...)
		if err == nil || !strings.Contains(err.Error(), "check") {
			t.Fatalf("empty value %v: err = %v, want a check violation", bad, err)
		}
	}
}
