package migration

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// migration-10: the script prompt's schema context resolves mixed-case
// names (to_regclass(format('%I.%I'))) through a bounded catalog read.
func TestTableSchema_MixedCaseNamesOnRealServer(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS "MixedCase" CASCADE`)
	})
	mustExecAll(t, ctx, pool,
		`DROP SCHEMA IF EXISTS "MixedCase" CASCADE`,
		`CREATE SCHEMA "MixedCase"`,
		`CREATE TABLE "MixedCase"."Orders" ("OrderID" int PRIMARY KEY,
			status text NOT NULL DEFAULT 'new')`,
		`CREATE INDEX "Orders_status_idx" ON "MixedCase"."Orders" (status)`)
	reader := catalogread.New(pool, catalogread.Default())
	got, err := queryTableSchema(ctx, reader, "MixedCase", "Orders")
	if err != nil {
		t.Fatalf("queryTableSchema: %v", err)
	}
	for _, want := range []string{
		"-- Table: MixedCase.Orders", "OrderID integer NOT NULL",
		"status text NOT NULL DEFAULT 'new'::text", "PRIMARY KEY (\"OrderID\")",
		"Orders_status_idx",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("schema context lacks %q:\n%s", want, got)
		}
	}

	gen := NewScriptGenerator(nil, pool, 160004, nopMigrationLog)
	if text := gen.fetchTableSchema(ctx, "MixedCase", "Orders"); !strings.Contains(text,
		"OrderID") {
		t.Fatalf("fetchTableSchema = %q, want the mixed-case table's columns", text)
	}
	// A missing table is no context, not an error.
	missing, err := queryTableSchema(ctx, reader, "MixedCase", "orders")
	if err != nil || strings.Contains(missing, "Columns") {
		t.Fatalf("missing table = %q, %v; want no columns and no error", missing, err)
	}
}
