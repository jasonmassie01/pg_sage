package schema

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Database IDs owned by this test; no other test uses this range.
const (
	windowCronActive     int64 = 990201
	windowShipped        int64 = 990202
	windowSuperseded     int64 = 990203
	windowAlreadyCurrent int64 = 990204
	windowProposed       int64 = 990205
	windowLegacyV1       int64 = 990206
)

// D2 T6: before schema version 3 a policy cron window matched one minute.
// The migration rewrites each cron entry to "<cron> @1m" so a stored
// document keeps its exact meaning under the one-hour default.
func TestPolicyWindowCronMigrationKeepsMeaning(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanupWindowRows(t, ctx, pool)
	t.Cleanup(func() { cleanupWindowRows(t, ctx, pool) })

	insertWindowPolicy(t, ctx, pool, windowCronActive, 2, "active",
		`["0 2 * * *", "weekdays 01:00-05:00", " */15  3 * * 1-5 "]`)
	insertWindowPolicy(t, ctx, pool, windowShipped, 2, "active", `["always", "weekends"]`)
	insertWindowPolicy(t, ctx, pool, windowSuperseded, 2, "superseded", `["0 2 * * *"]`)
	insertWindowPolicy(t, ctx, pool, windowAlreadyCurrent, 3, "active", `["0 2 * * *"]`)
	insertWindowPolicy(t, ctx, pool, windowProposed, 2, "proposed", `["30 4 * * 0"]`)
	// Version 1 rows belong to the class-split migration, which runs first
	// and lifts them to version 2; this migration leaves them alone.
	insertWindowPolicy(t, ctx, pool, windowLegacyV1, 1, "active", `["0 2 * * *"]`)

	for run := 0; run < 2; run++ { // second run proves idempotence
		if _, err := pool.Exec(ctx, ddlPolicyWindowCronDuration); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}

	assertWindows(t, ctx, pool, windowCronActive, 3,
		[]string{"0 2 * * * @1m", "weekdays 01:00-05:00", "*/15  3 * * 1-5 @1m"})
	assertWindows(t, ctx, pool, windowShipped, 3, []string{"always", "weekends"})
	assertWindows(t, ctx, pool, windowSuperseded, 2, []string{"0 2 * * *"})
	assertWindows(t, ctx, pool, windowAlreadyCurrent, 3, []string{"0 2 * * *"})
	assertWindows(t, ctx, pool, windowProposed, 3, []string{"30 4 * * 0 @1m"})
	assertWindows(t, ctx, pool, windowLegacyV1, 1, []string{"0 2 * * *"})
}

// The migrated document parses and matches exactly the minute it matched
// before: 02:00:30 is inside, 02:30 is not.
func TestPolicyWindowCronMigrationResultParses(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanupWindowRows(t, ctx, pool)
	t.Cleanup(func() { cleanupWindowRows(t, ctx, pool) })
	insertWindowPolicy(t, ctx, pool, windowCronActive, 2, "active", `["0 2 * * *"]`)
	if _, err := pool.Exec(ctx, ddlPolicyWindowCronDuration); err != nil {
		t.Fatalf("migration: %v", err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT doc FROM sage.policy WHERE database_id = $1`,
		windowCronActive).Scan(&raw); err != nil {
		t.Fatalf("read: %v", err)
	}
	doc, err := policy.ParseDocument(raw)
	if err != nil {
		t.Fatalf("migrated document does not parse: %v", err)
	}
	window, err := policy.ParseWindow(doc.MaintenanceWindows[0])
	if err != nil {
		t.Fatalf("migrated window %q: %v", doc.MaintenanceWindows[0], err)
	}
	inside := time.Date(2026, 9, 28, 2, 0, 30, 0, time.UTC)
	after := time.Date(2026, 9, 28, 2, 30, 0, 0, time.UTC)
	if !window.Contains(inside) || window.Contains(after) {
		t.Fatalf("migrated window changed meaning: 02:00:30=%v 02:30=%v, want true/false",
			window.Contains(inside), window.Contains(after))
	}
}

// Rows written after the migration are schema version 3 by default and are
// never rewritten: their cron entries already mean one hour.
func TestPolicyWindowNewRowsDefaultCurrent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanupWindowRows(t, ctx, pool)
	t.Cleanup(func() { cleanupWindowRows(t, ctx, pool) })
	if _, err := pool.Exec(ctx, `
INSERT INTO sage.policy (database_id, version, doc, status, proposed_by)
VALUES ($1, 1, $2, 'active', 'test')`, windowCronActive,
		windowDoc(t, `["0 2 * * *"]`)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := pool.Exec(ctx, ddlPolicyWindowCronDuration); err != nil {
		t.Fatalf("migration: %v", err)
	}
	assertWindows(t, ctx, pool, windowCronActive, 3, []string{"0 2 * * *"})
}

func windowDoc(t *testing.T, windows string) []byte {
	t.Helper()
	base, err := policy.MarshalDocument(policy.StaffedProfile())
	if err != nil {
		t.Fatalf("marshal base profile: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(base, &doc); err != nil {
		t.Fatalf("decode base profile: %v", err)
	}
	doc["maintenance_windows"] = json.RawMessage(windows)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode doc: %v", err)
	}
	return out
}

func insertWindowPolicy(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int64, schemaVersion int, status, windows string,
) {
	t.Helper()
	_, err := pool.Exec(ctx, `
INSERT INTO sage.policy (database_id, version, doc, status, proposed_by, schema_version)
VALUES ($1, 1, $2, $3, 'test', $4)`,
		databaseID, windowDoc(t, windows), status, schemaVersion)
	if err != nil {
		t.Fatalf("insert policy %d: %v", databaseID, err)
	}
}

func assertWindows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int64, wantVersion int, want []string,
) {
	t.Helper()
	var windows []string
	var version int
	err := pool.QueryRow(ctx, `
SELECT ARRAY(SELECT jsonb_array_elements_text(doc->'maintenance_windows')), schema_version
FROM sage.policy WHERE database_id = $1`, databaseID).Scan(&windows, &version)
	if err != nil {
		t.Fatalf("read policy %d: %v", databaseID, err)
	}
	if version != wantVersion || !slices.Equal(windows, want) {
		t.Errorf("policy %d = %q v%d, want %q v%d",
			databaseID, windows, version, want, wantVersion)
	}
}

func cleanupWindowRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.policy WHERE database_id = ANY($1)`,
		[]int64{windowCronActive, windowShipped, windowSuperseded,
			windowAlreadyCurrent, windowProposed, windowLegacyV1}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// The window migration is wired into bootstrap after the class split, so a
// version 1 row is split first and then gets its cron entries rewritten.
func TestPolicyWindowMigrationRunsAfterClassSplit(t *testing.T) {
	statements := migrationStatements()
	split := slices.Index(statements, ddlPolicyChangeClassSplit)
	window := slices.Index(statements, ddlPolicyWindowCronDuration)
	if split < 0 || window < 0 || window < split {
		t.Fatalf("class split at %d, window migration at %d; want both, window last",
			split, window)
	}
}
