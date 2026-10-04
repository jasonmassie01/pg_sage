package facts

import (
	"strings"
	"testing"
)

// A narrowing that redirects says so: the source-fix packet names the
// fact, who confirmed it and when, and for an app-owned object carries
// the migration pg_sage recommends instead of running the DDL itself.

func TestSourceFixForAnAppOwnedIndex(t *testing.T) {
	b := Binding{Fact: confirmed(12, TypeAppMigrations, KindTable, "public.thesis", nil),
		Object: "public.thesis", Route: RouteSourceFix}
	fix := BuildSourceFix(b, "CREATE INDEX CONCURRENTLY idx_t ON public.thesis (run_id)",
		"DROP INDEX CONCURRENTLY public.idx_t")
	if fix.FactID != 12 || fix.Route != string(RouteSourceFix) ||
		fix.Object != "public.thesis" ||
		fix.Provenance != "fact #12, confirmed by alice@example.com on 2026-10-03" {
		t.Fatalf("packet %+v", fix)
	}
	for _, want := range []string{"fact #12", "owned by the application's migrations",
		"CREATE INDEX CONCURRENTLY idx_t ON public.thesis (run_id);",
		"non-transactional"} {
		if !strings.Contains(fix.Migration, want) {
			t.Fatalf("migration lacks %q:\n%s", want, fix.Migration)
		}
	}
	if fix.Down != "DROP INDEX CONCURRENTLY public.idx_t;" {
		t.Fatalf("down = %q", fix.Down)
	}
	if !strings.Contains(fix.Summary, "pg_sage will not run this DDL") ||
		!strings.Contains(fix.Summary, "application's migrations") {
		t.Fatalf("summary = %q", fix.Summary)
	}
}

func TestSourceFixOtherRoutesCarryNoMigration(t *testing.T) {
	cases := []struct {
		b    Binding
		want string
	}{
		{Binding{Fact: confirmed(5, TypeAppendOnly, KindTable, "app.audit", nil),
			Object: "app.audit", Route: RouteKeep}, "kept"},
		{Binding{Fact: confirmed(9, TypeSlotConsumer, KindSlot, "cdc",
			map[string]string{"consumer": "debezium"}), Object: "slot:cdc",
			Route: RouteAlert}, "alerts"},
		{Binding{Fact: confirmed(7, TypeTestFixture, KindSchema, "test_*", nil),
			Object: "test_a.t", Route: RouteExcluded}, "test fixture"},
		{Binding{Fact: confirmed(8, TypeTableWindow, KindTable, "app.e",
			map[string]string{"kind": "batch", "window": "daily 01:00-02:00"}),
			Object: "app.e", Route: RouteWait}, "window"},
	}
	for _, c := range cases {
		fix := BuildSourceFix(c.b, "DELETE FROM app.audit", "")
		if fix.Migration != "" || fix.Down != "" {
			t.Fatalf("%s: a non-DDL route carries a migration: %+v", c.b.Route, fix)
		}
		if !strings.Contains(fix.Summary, c.want) ||
			!strings.Contains(fix.Summary, c.b.Fact.Provenance()) {
			t.Fatalf("%s summary = %q", c.b.Route, fix.Summary)
		}
	}
}
