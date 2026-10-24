package schema

import (
	"strings"
	"testing"
)

// The receipts' additions to sage.change_review: landing_checked_at (the
// last receipts check that saw the change not landed; the lower end of a
// catalog landing bracket) and change_review_shadow_due, the partial index
// of flagged reviews whose shadow score is settled but not yet recorded.
// TestChangeReviewMigrationIsIdempotent covers both on a second Bootstrap.

func TestChangeReviewReceiptsMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	var typ string
	var notNull bool
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull
		FROM pg_attribute WHERE attrelid = 'sage.change_review'::regclass
		  AND attname = 'landing_checked_at' AND NOT attisdropped`).
		Scan(&typ, &notNull); err != nil {
		t.Fatalf("landing_checked_at: %v", err)
	}
	if typ != "timestamp with time zone" || notNull {
		t.Fatalf("landing_checked_at is %s (not null %v), want nullable timestamptz", typ,
			notNull)
	}
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_indexdef('sage.change_review_shadow_due'::regclass)`).
		Scan(&def); err != nil {
		t.Fatalf("change_review_shadow_due: %v", err)
	}
	for _, want := range []string{"WHERE", "shadow_auto_clear", "'unscored'", "'regressed'",
		"'override'", "'concur_with_edits'", "'clean_low_traffic'"} {
		if !strings.Contains(def, want) {
			t.Errorf("change_review_shadow_due = %q, want it to name %s", def, want)
		}
	}
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_indexdef('sage.change_review_landing_rotation'::regclass)`).
		Scan(&def); err != nil {
		t.Fatalf("change_review_landing_rotation: %v", err)
	}
	for _, want := range []string{"landing_checked_at NULLS FIRST", "completed_at",
		"WHERE (landing_state = 'awaiting'"} {
		if !strings.Contains(def, want) {
			t.Errorf("change_review_landing_rotation = %q, want it to name %s", def, want)
		}
	}
}
