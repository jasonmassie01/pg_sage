package schema

import (
	"context"
	"strings"
	"testing"
)

// ux-02b: sage.briefings gains digest_hash, and sage.digest_delivery keeps
// the last delivered digest hash per scope, so the send check is one
// INSERT ... ON CONFLICT. The concurrent send is covered by
// briefing.TestDigestDB_TwoFlapsSendOnce.

// Other T5 steps follow it once the T5 chains merge, so its place is
// pinned after ddlWorkloadPseudonymKey rather than "last".
func TestBriefingsDigestHashStep_IsInPlaceBoundedAndIdempotent(t *testing.T) {
	steps := trancheFiveMigrations()
	at, after := -1, -1
	for i, s := range steps {
		switch s[0] {
		case "ddlBriefingsDigestHash":
			at = i
		case "ddlWorkloadPseudonymKey":
			after = i
		}
	}
	if at < 0 || after < 0 || at < after {
		t.Fatalf("T5 steps = %v, want ddlBriefingsDigestHash after ddlWorkloadPseudonymKey",
			steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ALTER TABLE sage.briefings ADD COLUMN IF NOT EXISTS digest_hash text",
		"CREATE TABLE IF NOT EXISTS sage.digest_delivery", "scope       text PRIMARY KEY",
		"CHECK (digest_hash ~ '^[0-9a-f]{64}$')"} {
		if !strings.Contains(ddlBriefingsDigestHash, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlBriefingsDigestHash) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

// Integration: two bootstraps leave the column and the table, and the
// claim statement answers once per hash.
func TestBriefingsDigestHashMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "digesthash")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var typ string
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		WHERE attrelid = 'sage.briefings'::regclass AND attname = 'digest_hash'`).
		Scan(&typ); err != nil || typ != "text" {
		t.Fatalf("digest_hash type = %q, %v; want text", typ, err)
	}
	hash := strings.Repeat("a", 64)
	claim := `INSERT INTO sage.digest_delivery AS d (scope, digest_hash)
		VALUES ('db:prod', $1) ON CONFLICT (scope) DO UPDATE
		SET digest_hash = EXCLUDED.digest_hash, delivered_at = now()
		WHERE d.digest_hash IS DISTINCT FROM EXCLUDED.digest_hash RETURNING scope`
	for i, want := range []int64{1, 0} {
		tag, err := pool.Exec(ctx, claim, hash)
		if err != nil || tag.RowsAffected() != want {
			t.Fatalf("claim %d: %v rows, %v; want %d", i, tag.RowsAffected(), err, want)
		}
	}
	if _, err := pool.Exec(ctx, claim, "not-a-hash"); err == nil {
		t.Fatal("a malformed hash was accepted")
	}
}
