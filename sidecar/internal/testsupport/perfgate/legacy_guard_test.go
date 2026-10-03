package perfgate

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// The leaked copies form one clone family, and the legacy rows carry the
// identity the schema guard gives its everything_text invariant.
func TestLeakedClonesAreOneFamilyWithTheGuardsIdentity(t *testing.T) {
	schemas := leakedSchemas()
	if len(schemas) != leakedCloneSchemas || leakedTargetsPerRow >= leakedCloneSchemas {
		t.Fatalf("%d leaked schemas, %d per row: want more schemas than targets per row",
			len(schemas), leakedTargetsPerRow)
	}
	shapes := make([]schemaguard.SchemaShape, 0, len(schemas))
	for _, sch := range schemas {
		if schemaguard.CloneStem(sch) != "test_leak_" {
			t.Fatalf("schema %s has clone stem %q, want test_leak_", sch,
				schemaguard.CloneStem(sch))
		}
		shapes = append(shapes, schemaguard.SchemaShape{Schema: sch,
			Tables: []string{leakedTable}})
	}
	family := schemaguard.GroupFamilies(shapes)[schemas[len(schemas)-1]]
	if family == nil || len(family.Members) != leakedCloneSchemas {
		t.Fatalf("leaked schemas family = %+v, want all %d members", family,
			leakedCloneSchemas)
	}
	want := schemaguard.InvariantIdentity(schemaguard.Invariant{
		Kind: schemaguard.InvariantEverythingText, Schema: schemas[3], Table: leakedTable,
		Family: family})
	if got := leakedIdentity(); got != want || len(got) != 32 {
		t.Fatalf("leaked identity = %q, want %q", got, want)
	}
}

// The catalog holds the leaked copies, each with an all-text table (an
// everything_text invariant), outside the perf_ catalog the scale counts.
func TestBuildCatalogCreatesTheLeakedCopies(t *testing.T) {
	pool, ctx := livePool(t)
	for run := 0; run < 2; run++ {
		if err := BuildCatalog(ctx, pool, tinyScale()); err != nil {
			t.Fatalf("build run %d: %v", run, err)
		}
	}
	tables := count(t, ctx, pool, `SELECT count(*) FROM pg_class c JOIN pg_namespace n
		ON n.oid = c.relnamespace WHERE c.relkind = 'r' AND c.relname = $1
		AND n.nspname LIKE 'test\_leak\_%' AND (SELECT count(*) FROM pg_attribute a
		WHERE a.attrelid = c.oid AND a.attnum > 0 AND a.atttypid = 'text'::regtype) = 3`,
		leakedTable)
	if tables != leakedCloneSchemas {
		t.Fatalf("leaked all-text tables = %d, want %d", tables, leakedCloneSchemas)
	}
}

// The legacy rows: HistoryRows / legacyGuardShare schema guard rows, each
// naming leakedTargetsPerRow copies, under the guard's identity, in the
// last day (inside decision retention, as on lifeos).
func TestSeedHistoryWritesTheLegacyGuardFlood(t *testing.T) {
	pool, ctx, _ := seededPool(t)
	want := int64(tinyScale().HistoryRows / legacyGuardShare)
	rows := count(t, ctx, pool, `SELECT count(*) FROM sage.decision
		WHERE feature = 'schema_guard' AND evidence->>'invariant_key' = $1
		AND jsonb_array_length(target_objects) = $2
		AND target_objects ? ($3::text || '.' || $4::text)
		AND created_at > now() - interval '1 day'`,
		leakedIdentity(), leakedTargetsPerRow, leakedSchema(leakedCloneSchemas-1), leakedTable)
	all := count(t, ctx, pool, `SELECT count(*) FROM sage.decision
		WHERE evidence->>'invariant_key' = $1`, leakedIdentity())
	if all != want || rows == 0 || rows == want {
		t.Fatalf("legacy rows = %d (%d naming the last copy), want %d spread over "+
			"drifting windows", all, rows, want)
	}
}
