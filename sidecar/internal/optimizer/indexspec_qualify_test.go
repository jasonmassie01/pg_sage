package optimizer

import (
	"strings"
	"testing"
)

// No concurrent access tests: qualifyIndexTable and canonicalizeRecommendation
// are pure functions of their inputs.

// The header refactor keeps ParseIndexDDL's results: same spec for the
// statement shapes ParseIndexDDL documents.
func TestParseIndexDDLUnchangedByHeaderScan(t *testing.T) {
	spec, err := ParseIndexDDL(`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "Idx" ON ONLY ` +
		`app."Orders" USING GIN (a gin_trgm_ops) INCLUDE (b) WHERE a IS NOT NULL;`)
	if err != nil {
		t.Fatalf("ParseIndexDDL: %v", err)
	}
	want := IndexSpec{Unique: true, Name: "Idx", TableSchema: "app", TableName: "Orders",
		Method: "gin", Keys: "a gin_trgm_ops", Include: "b", Where: "a is not null"}
	if spec != want {
		t.Fatalf("spec = %+v, want %+v", spec, want)
	}
}

// qualifyIndexTable names the schema on an unqualified target and leaves
// every other byte of the statement as it was.
func TestQualifyIndexTable(t *testing.T) {
	cases := []struct{ ddl, schema, want string }{
		{"CREATE INDEX CONCURRENTLY idx ON orders (status)", "public",
			`CREATE INDEX CONCURRENTLY idx ON "public".orders (status)`},
		{"CREATE INDEX idx ON ONLY orders USING gin (a gin_trgm_ops);", "public",
			`CREATE INDEX idx ON ONLY "public".orders USING gin (a gin_trgm_ops);`},
		{`CREATE INDEX idx ON "Orders" (status)`, "public",
			`CREATE INDEX idx ON "public"."Orders" (status)`},
		{"CREATE INDEX idx ON orders (status)", `we"ird`,
			`CREATE INDEX idx ON "we""ird".orders (status)`},
		// Already qualified: unchanged byte for byte.
		{"CREATE INDEX  idx ON  app.orders  (status)", "public",
			"CREATE INDEX  idx ON  app.orders  (status)"},
	}
	for _, c := range cases {
		got, err := qualifyIndexTable(c.ddl, c.schema)
		if err != nil || got != c.want {
			t.Errorf("qualifyIndexTable(%q, %q) = %q, %v; want %q", c.ddl, c.schema, got, err,
				c.want)
		}
	}
	if _, err := qualifyIndexTable("DROP INDEX public.i", "public"); err == nil {
		t.Error("qualifyIndexTable accepted a DROP INDEX")
	}
}

// The optimizer's DDL always names the analyzed table's schema, so it
// resolves in pg_sage's pinned DDL session; identity, rejection shape and
// rollback are unchanged by the qualification.
func TestCanonicalizeQualifiesUnqualifiedTable(t *testing.T) {
	rec := sampleRecommendation()
	rec.DDL = "CREATE INDEX CONCURRENTLY idx_orders_status ON orders (status)"
	before := rec.FindingIdentifier()
	beforeShape, err := shapeOfCandidate(rec.DDL)
	if err != nil {
		t.Fatalf("shape before: %v", err)
	}
	got, err := canonicalizeRecommendation(rec, sampleTableContext())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	want := `CREATE INDEX CONCURRENTLY idx_orders_status ON "public".orders (status)`
	if got.DDL != want {
		t.Fatalf("DDL = %q, want %q", got.DDL, want)
	}
	spec, err := ParseIndexDDL(got.DDL)
	if err != nil || spec.TableSchema != "public" || spec.TableName != "orders" {
		t.Fatalf("canonical DDL parses as %+v, %v", spec, err)
	}
	if got.FindingIdentifier() != before {
		t.Errorf("identity changed: %q -> %q", before, got.FindingIdentifier())
	}
	afterShape, err := shapeOfCandidate(got.DDL)
	if err != nil || afterShape.hash() != beforeShape.hash() {
		t.Errorf("rejection shape changed: %v -> %v (%v)", beforeShape, afterShape, err)
	}
	if got.DropDDL != `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_orders_status"` {
		t.Errorf("DropDDL = %q", got.DropDDL)
	}
}

// A qualified DDL is not rewritten, and an unqualified DDL for a table
// outside public is still refused (it would resolve to another table).
func TestCanonicalizeKeepsQualifiedAndRefusesOtherSchemas(t *testing.T) {
	rec := sampleRecommendation()
	got, err := canonicalizeRecommendation(rec, sampleTableContext())
	if err != nil || got.DDL != rec.DDL {
		t.Fatalf("qualified DDL rewritten to %q, %v; want %q", got.DDL, err, rec.DDL)
	}
	tc := sampleTableContext()
	tc.Schema = "app"
	rec.DDL = "CREATE INDEX CONCURRENTLY idx_orders_status ON orders (status)"
	if _, err := canonicalizeRecommendation(rec, tc); err == nil ||
		!strings.Contains(err.Error(), "not the analyzed table") {
		t.Fatalf("unqualified DDL for app.orders: %v, want refusal", err)
	}
}
