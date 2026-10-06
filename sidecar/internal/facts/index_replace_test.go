package facts

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// A replace (roadmap 2.3) is DDL on its table and drops an index: a table
// owned by the application's migrations gets the two-statement migration
// and its down instead of DDL, and an append-only table keeps its indexes.

func replacePair() (string, string) {
	create := "CREATE INDEX CONCURRENTLY orders_status_created ON public.orders " +
		"(status, created_at)"
	old := "CREATE INDEX orders_status_idx ON public.orders USING btree (status)"
	return optimizer.IndexReplaceSQL(create, "public.orders_status_idx"),
		optimizer.IndexReplaceRollbackSQL(old, "public.orders_status_created")
}

func TestBindIndexReplace(t *testing.T) {
	sql, _ := replacePair()
	owned := confirmed(1, TypeAppMigrations, KindTable, "public.orders", nil)
	got := bindSQL([]Fact{owned}, "replace_index", sql, true, "public.orders",
		"public.orders_status_idx")
	if len(got) != 1 || got[0].Route != RouteSourceFix {
		t.Fatalf("a migrations-owned table binds the replace to a source fix: %+v", got)
	}
	appendOnly := confirmed(2, TypeAppendOnly, KindTable, "public.orders", nil)
	got = bindSQL([]Fact{appendOnly}, "replace_index", sql, true, "public.orders",
		"public.orders_status_idx")
	if len(got) != 1 || got[0].Route != RouteKeep {
		t.Fatalf("an append-only table keeps its indexes: %+v", got)
	}
	other := confirmed(3, TypeAppMigrations, KindTable, "public.items", nil)
	if got := bindSQL([]Fact{other}, "replace_index", sql, true, "public.orders",
		"public.orders_status_idx"); len(got) != 0 {
		t.Fatalf("a fact on another table does not bind: %+v", got)
	}
}

func TestRedirectIndexReplaceAttachesTheTwoStatementMigration(t *testing.T) {
	sql, rollback := replacePair()
	f := analyzer.Finding{Category: "tuning_index_replace", ObjectType: "index",
		ObjectIdentifier: "public.orders", RecommendedSQL: sql, RollbackSQL: rollback,
		ActionRisk: "moderate", Detail: map[string]any{"table": "public.orders"}}
	owned := confirmed(1, TypeAppMigrations, KindTable, "public.orders", nil)
	out, _ := ApplyFindings([]Fact{owned}, []analyzer.Finding{f}, nil, bindNow)
	if len(out) != 1 || out[0].RecommendedSQL != "" || out[0].RollbackSQL != "" {
		t.Fatalf("the DDL is withheld: %+v", out)
	}
	fix, ok := out[0].Detail["source_fix"].(SourceFix)
	if !ok {
		t.Fatalf("source fix = %#v", out[0].Detail["source_fix"])
	}
	for _, want := range []string{"non-transactional",
		"CREATE INDEX CONCURRENTLY orders_status_created",
		"DROP INDEX CONCURRENTLY public.orders_status_idx;"} {
		if !strings.Contains(fix.Migration, want) {
			t.Fatalf("migration lacks %q:\n%s", want, fix.Migration)
		}
	}
	for _, want := range []string{"CREATE INDEX CONCURRENTLY orders_status_idx",
		"DROP INDEX CONCURRENTLY IF EXISTS public.orders_status_created;"} {
		if !strings.Contains(fix.Down, want) {
			t.Fatalf("down lacks %q:\n%s", want, fix.Down)
		}
	}
	if strings.Contains(fix.Migration, ";;") || strings.Contains(fix.Down, ";;") {
		t.Fatalf("each statement ends with exactly one semicolon:\n%s\n%s",
			fix.Migration, fix.Down)
	}
}
