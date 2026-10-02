package analyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Dogfood lifeos-1 finding 9c: pg_sage dropped public.idx_thesis_allocation_run
// 8 times in 40 minutes; the application's migrations recreated it each
// time and v1.8.0 proposed the drop again. An index pg_sage dropped that
// is back with the same definition belongs to the application: pg_sage
// stops proposing to drop it and reports that instead.

func dropFinding(category, ident string) Finding {
	return Finding{Category: category, Severity: "warning", ObjectType: "index",
		ObjectIdentifier: ident, Title: "Duplicate index " + ident,
		RecommendedSQL: "DROP INDEX CONCURRENTLY " + ident + ";",
		RollbackSQL:    "CREATE INDEX ...", ActionRisk: "safe"}
}

func TestMarkAppManaged_ConvertsDropFindings(t *testing.T) {
	last := time.Date(2026, 6, 12, 3, 7, 11, 0, time.UTC)
	managed := map[string]appManagedIndex{
		"public.idx_thesis_allocation_run": {Drops: 8, LastDrop: last}}
	in := []Finding{
		dropFinding("duplicate_index", "public.idx_thesis_allocation_run"),
		dropFinding("unused_index", "public.idx_thesis_allocation_run"),
		dropFinding("duplicate_index", "public.other_idx"),
		{Category: "slow_query", ObjectIdentifier: "public.idx_thesis_allocation_run"},
	}
	out := markAppManaged(in, managed)
	if len(out) != 3 {
		t.Fatalf("findings = %d, want the two drop findings folded into one: %+v",
			len(out), out)
	}
	var got *Finding
	for i := range out {
		if out[i].Category == CategoryAppManagedIndex {
			got = &out[i]
		}
	}
	if got == nil || got.RecommendedSQL != "" || got.RollbackSQL != "" ||
		got.ObjectIdentifier != "public.idx_thesis_allocation_run" ||
		got.Detail["drops"] != 8 || got.Severity != "info" ||
		!strings.Contains(got.Recommendation, "migrations") ||
		!strings.Contains(got.Title, "8") {
		t.Fatalf("app-managed finding = %+v", got)
	}
	cats := map[string]int{}
	for _, f := range out {
		cats[f.Category]++
	}
	if cats["duplicate_index"] != 1 || cats["slow_query"] != 1 {
		t.Fatalf("categories = %v, want the unrelated findings untouched", cats)
	}
}

func TestMarkAppManaged_EmptyInputs(t *testing.T) {
	if out := markAppManaged(nil, nil); len(out) != 0 {
		t.Fatalf("nil inputs = %+v", out)
	}
	in := []Finding{dropFinding("duplicate_index", "public.x")}
	if out := markAppManaged(in, nil); len(out) != 1 || out[0].RecommendedSQL == "" {
		t.Fatalf("no history changed findings: %+v", out)
	}
}

// Against PostgreSQL: the history comes from sage.action_log. Only
// executed drops count (a drop pg_sage rolled back itself is not the
// application's doing), and only when the index is back with the same
// definition (a changed definition is a new index).
func TestLoadAppManagedIndexes_FromActionLog(t *testing.T) {
	p, ctx := preflightPool(t)
	sfx := time.Now().UnixNano()
	tbl := fmt.Sprintf("am_%d", sfx)
	preflightSQL(t, p, fmt.Sprintf(`CREATE TABLE public.%[1]s (a int, b int);
		CREATE INDEX %[1]s_app ON public.%[1]s (a, b);
		CREATE INDEX %[1]s_own ON public.%[1]s (a);
		CREATE INDEX %[1]s_chg ON public.%[1]s (b)`, tbl))
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), "DROP TABLE IF EXISTS public."+tbl)
		_, _ = p.Exec(context.Background(),
			"DELETE FROM sage.action_log WHERE sql_executed LIKE $1", "%"+tbl+"%")
	})
	drop := func(index, rollback, outcome string) {
		t.Helper()
		if _, err := p.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
			rollback_sql, outcome, executed_at)
			VALUES ('drop_index', $1, $2, $3, now() - interval '2 days')`,
			"DROP INDEX CONCURRENTLY public."+index+";", rollback, outcome); err != nil {
			t.Fatalf("action: %v", err)
		}
	}
	def := func(index, cols string) string {
		return fmt.Sprintf("CREATE INDEX %s ON public.%s USING btree (%s)", index, tbl, cols)
	}
	drop(tbl+"_app", def(tbl+"_app", "a, b"), "success")
	drop(tbl+"_app", def(tbl+"_app", "a, b"), "rollback_failed")
	drop(tbl+"_own", def(tbl+"_own", "a"), "rolled_back")
	drop(tbl+"_chg", def(tbl+"_chg", "a"), "success") // recreated on another column
	drop(tbl+"_gone", def(tbl+"_gone", "a"), "success")
	got, err := loadAppManagedIndexes(ctx, p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	app, ok := got["public."+tbl+"_app"]
	if !ok || app.Drops != 2 || app.LastDrop.IsZero() {
		t.Fatalf("app-managed = %+v (%v), want 2 drops of %s_app", app, ok, tbl)
	}
	for _, s := range []string{"_own", "_chg", "_gone"} {
		if _, ok := got["public."+tbl+s]; ok {
			t.Errorf("%s%s marked app-managed", tbl, s)
		}
	}
}
