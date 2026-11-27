package schema

import (
	"context"
	"strings"
	"testing"
)

// earned-02: sage.trust_ledger_state gains the incident reconciler's
// cursors, one pair per kind (handoff_cursor / handoff_pending,
// auto_cursor / auto_pending). Additive, nullable (a NULL cursor reads
// from the 30-day lookback, as before the upgrade) and idempotent.

const incidentCursorDeployment = "74747474-7474-4474-8474-747474747474"

var incidentCursorColumns = map[string]string{
	"handoff_cursor":  "bigint",
	"handoff_pending": "bigint[]",
	"auto_cursor":     "bigint",
	"auto_pending":    "bigint[]",
}

func requireIncidentCursorColumns(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := requireDB(t)
	for column, want := range incidentCursorColumns {
		var typ string
		var notNull bool
		var n int
		if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull,
			count(*) OVER ()
			FROM pg_attribute WHERE attrelid = 'sage.trust_ledger_state'::regclass
			  AND attname = $1 AND NOT attisdropped`, column).Scan(&typ, &notNull,
			&n); err != nil {
			t.Fatalf("column %s: %v", column, err)
		}
		if typ != want || notNull || n != 1 {
			t.Fatalf("column %s is %s (not null %v, %d copies), want nullable %s",
				column, typ, notNull, n, want)
		}
	}
}

func TestTrustLedgerIncidentCursorMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireIncidentCursorColumns(t, ctx)
}

// An install from before earned-02 keeps its row: the columns arrive NULL
// (the first pass reads from the lookback) and the self-initiated
// cursors are untouched.
func TestTrustLedgerIncidentCursorMigrationUpgradesAnExistingRow(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.trust_ledger_state WHERE deployment_id = $1",
			incidentCursorDeployment)
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.trust_ledger_state
		DROP COLUMN IF EXISTS handoff_cursor, DROP COLUMN IF EXISTS handoff_pending,
		DROP COLUMN IF EXISTS auto_cursor, DROP COLUMN IF EXISTS auto_pending`); err != nil {
		t.Fatalf("simulating the pre-earned-02 table: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.trust_ledger_state (deployment_id,
		database_name, verdict_cursor) VALUES ($1, 'orders', '2026-11-01T00:00:00Z')`,
		incidentCursorDeployment); err != nil {
		t.Fatalf("seeding a pre-upgrade row: %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	requireIncidentCursorColumns(t, ctx)
	var verdictKept, allNull bool
	if err := pool.QueryRow(ctx, `SELECT verdict_cursor = '2026-11-01T00:00:00Z',
		handoff_cursor IS NULL AND handoff_pending IS NULL AND auto_cursor IS NULL
		  AND auto_pending IS NULL
		FROM sage.trust_ledger_state WHERE deployment_id = $1`,
		incidentCursorDeployment).Scan(&verdictKept, &allNull); err != nil {
		t.Fatalf("reading the upgraded row: %v", err)
	}
	if !verdictKept || !allNull {
		t.Fatalf("upgraded row: verdict cursor kept %v, new cursors NULL %v", verdictKept,
			allNull)
	}
}

// The migration names its lock bound and only alters the table when the
// catalog lacks a column, so a re-run takes no lock on it.
func TestTrustLedgerIncidentCursorMigrationIsBounded(t *testing.T) {
	ddl := ddlTrustLedgerIncidentCursors
	for _, fragment := range []string{"SET LOCAL lock_timeout", "pg_attribute",
		"ADD COLUMN IF NOT EXISTS handoff_cursor bigint",
		"ADD COLUMN IF NOT EXISTS auto_pending bigint[]"} {
		if !strings.Contains(ddl, fragment) {
			t.Fatalf("migration lacks %q:\n%s", fragment, ddl)
		}
	}
	ledger, cursors := -1, -1
	for i, stmt := range migrationStatements() {
		switch stmt {
		case ddlTrustLedger():
			ledger = i
		case ddlTrustLedgerIncidentCursors:
			cursors = i
		}
	}
	if ledger < 0 || cursors <= ledger {
		t.Fatalf("chain positions: trust ledger %d, incident cursors %d; the cursors "+
			"need the table", ledger, cursors)
	}
}
