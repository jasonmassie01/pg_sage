package schema

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Database IDs owned by this test; no other test uses this range.
const (
	splitIndexAllowed   int64 = 990101
	splitIndexDenied    int64 = 990102
	splitAlreadyCurrent int64 = 990103
	splitSuperseded     int64 = 990104
)

var splitClasses = []string{"backend_signal", "query_hint", "schema_change"}

// Before G4-B18, backend signals, query hints and schema changes were
// authorized as change class "index". Stored v1 documents must keep that
// effective permission: the migration grants the split classes exactly where
// "index" was allowed (and requires approval where "index" did).
func TestPolicyChangeClassSplitMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanupSplitRows(t, ctx, pool)
	t.Cleanup(func() { cleanupSplitRows(t, ctx, pool) })

	insertSplitPolicy(t, ctx, pool, splitIndexAllowed, 1, "active",
		`["index","vacuum"]`, `["index"]`)
	insertSplitPolicy(t, ctx, pool, splitIndexDenied, 1, "active",
		`["vacuum","analyze"]`, `[]`)
	insertSplitPolicy(t, ctx, pool, splitAlreadyCurrent, 2, "active",
		`["index"]`, `[]`)
	insertSplitPolicy(t, ctx, pool, splitSuperseded, 1, "superseded",
		`["index"]`, `[]`)

	for run := 0; run < 2; run++ { // second run proves idempotence
		if _, err := pool.Exec(ctx, ddlPolicyChangeClassSplit); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}

	allowed, approval, version := readSplitPolicy(t, ctx, pool, splitIndexAllowed)
	if want := []string{"index", "vacuum", "backend_signal", "query_hint",
		"schema_change"}; !slices.Equal(allowed, want) {
		t.Errorf("index-allowed doc classes = %v, want %v", allowed, want)
	}
	if want := []string{"index", "backend_signal", "query_hint",
		"schema_change"}; !slices.Equal(approval, want) {
		t.Errorf("index-allowed approval classes = %v, want %v", approval, want)
	}
	if version != 2 {
		t.Errorf("index-allowed schema_version = %d, want 2", version)
	}

	allowed, _, version = readSplitPolicy(t, ctx, pool, splitIndexDenied)
	if !slices.Equal(allowed, []string{"vacuum", "analyze"}) || version != 2 {
		t.Errorf("index-denied doc = %v v%d, want unchanged classes at v2", allowed, version)
	}
	allowed, _, _ = readSplitPolicy(t, ctx, pool, splitAlreadyCurrent)
	if !slices.Equal(allowed, []string{"index"}) {
		t.Errorf("v2 doc was rewritten: %v", allowed)
	}
	allowed, _, version = readSplitPolicy(t, ctx, pool, splitSuperseded)
	if !slices.Equal(allowed, []string{"index"}) || version != 1 {
		t.Errorf("superseded history was rewritten: %v v%d", allowed, version)
	}
}

// Rows written after the migration are current by default and are never
// widened by a later run.
func TestPolicyChangeClassSplitNewRowsDefaultCurrent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanupSplitRows(t, ctx, pool)
	t.Cleanup(func() { cleanupSplitRows(t, ctx, pool) })

	doc := splitDoc(t, `["index"]`, `[]`)
	if _, err := pool.Exec(ctx, `
INSERT INTO sage.policy (database_id, version, doc, status, proposed_by)
VALUES ($1, 1, $2, 'active', 'test')`, splitIndexAllowed, doc); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := pool.Exec(ctx, ddlPolicyChangeClassSplit); err != nil {
		t.Fatalf("migration: %v", err)
	}
	allowed, _, version := readSplitPolicy(t, ctx, pool, splitIndexAllowed)
	if version != 2 || !slices.Equal(allowed, []string{"index"}) {
		t.Errorf("new row = %v v%d, want [index] at v2", allowed, version)
	}
}

// The migrated document must still parse as a valid policy document.
func TestPolicyChangeClassSplitResultParses(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanupSplitRows(t, ctx, pool)
	t.Cleanup(func() { cleanupSplitRows(t, ctx, pool) })

	insertSplitPolicy(t, ctx, pool, splitIndexAllowed, 1, "active", `["index"]`, `[]`)
	if _, err := pool.Exec(ctx, ddlPolicyChangeClassSplit); err != nil {
		t.Fatalf("migration: %v", err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT doc FROM sage.policy WHERE database_id = $1`,
		splitIndexAllowed).Scan(&raw); err != nil {
		t.Fatalf("read: %v", err)
	}
	doc, err := policy.ParseDocument(raw)
	if err != nil {
		t.Fatalf("migrated document does not parse: %v", err)
	}
	for _, class := range splitClasses {
		if !slices.Contains(doc.AllowedChangeClasses, policy.ChangeClass(class)) {
			t.Errorf("parsed document lacks %q", class)
		}
	}
}

func splitDoc(t *testing.T, allowed, approval string) []byte {
	t.Helper()
	base, err := policy.MarshalDocument(policy.StaffedProfile())
	if err != nil {
		t.Fatalf("marshal base profile: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(base, &doc); err != nil {
		t.Fatalf("decode base profile: %v", err)
	}
	doc["allowed_change_classes"] = json.RawMessage(allowed)
	doc["approval_required_classes"] = json.RawMessage(approval)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode doc: %v", err)
	}
	return out
}

func insertSplitPolicy(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int64, schemaVersion int, status, allowed, approval string,
) {
	t.Helper()
	_, err := pool.Exec(ctx, `
INSERT INTO sage.policy (database_id, version, doc, status, proposed_by, schema_version)
VALUES ($1, 1, $2, $3, 'test', $4)`,
		databaseID, splitDoc(t, allowed, approval), status, schemaVersion)
	if err != nil {
		t.Fatalf("insert policy %d: %v", databaseID, err)
	}
}

func readSplitPolicy(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, databaseID int64,
) ([]string, []string, int) {
	t.Helper()
	var allowed, approval []string
	var version int
	err := pool.QueryRow(ctx, `
SELECT ARRAY(SELECT jsonb_array_elements_text(doc->'allowed_change_classes')),
       ARRAY(SELECT jsonb_array_elements_text(
           COALESCE(doc->'approval_required_classes', '[]'::jsonb))),
       schema_version
FROM sage.policy WHERE database_id = $1`, databaseID).Scan(&allowed, &approval, &version)
	if err != nil {
		t.Fatalf("read policy %d: %v", databaseID, err)
	}
	return allowed, approval, version
}

func cleanupSplitRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.policy WHERE database_id = ANY($1)`,
		[]int64{splitIndexAllowed, splitIndexDenied, splitAlreadyCurrent,
			splitSuperseded}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
