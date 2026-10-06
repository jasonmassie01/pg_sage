package approvalcard

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// A replace card shows both statements it approves (the build of the wider
// index and the drop of the subsumed one), the undo (re-create the old
// index, drop the new one), the locks both steps take, and both indexes
// as targets. The content hash covers both statements, so an approval
// binds to the pair it showed.

func replaceQueued() (string, string) {
	create := "CREATE INDEX CONCURRENTLY orders_status_created ON public.orders " +
		"(status, created_at)"
	old := "CREATE INDEX orders_status_idx ON public.orders USING btree (status)"
	return optimizer.IndexReplaceSQL(create, "public.orders_status_idx"),
		optimizer.IndexReplaceRollbackSQL(old, "public.orders_status_created")
}

func TestAssembleIndexReplaceShowsBothStatementsUndoAndLocks(t *testing.T) {
	sql, rollback := replaceQueued()
	a := queued(9, "replace_index", sql, rollback, "moderate")
	c := Assemble(Inputs{Database: "orders", Action: a, Now: now, Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Category: "tuning_index_replace",
			ObjectType: "index", Object: "public.orders",
			Title: "Replace public.orders_status_idx with public.orders_status_created",
			Detail: map[string]any{"table": "public.orders",
				"index_replace": map[string]any{"old_index": "public.orders_status_idx",
					"new_index": "public.orders_status_created"}}}})
	if c.ActionType != "replace_index" || c.SQL != sql {
		t.Fatalf("card SQL = %q (%s)", c.SQL, c.ActionType)
	}
	for _, want := range []string{"CREATE INDEX CONCURRENTLY orders_status_created",
		"DROP INDEX CONCURRENTLY public.orders_status_idx"} {
		if !strings.Contains(c.SQL, want) {
			t.Fatalf("card SQL lacks %q: %q", want, c.SQL)
		}
	}
	if c.Rollback.SQL != rollback || c.Rollback.Class != "reversible" ||
		!strings.Contains(c.Rollback.SQL, "CREATE INDEX CONCURRENTLY orders_status_idx") ||
		!strings.Contains(c.Rollback.SQL, "DROP INDEX CONCURRENTLY IF EXISTS "+
			"public.orders_status_created") {
		t.Fatalf("rollback = %+v", c.Rollback)
	}
	if !strings.Contains(c.Risk.Lock, "SHARE UPDATE EXCLUSIVE") ||
		!strings.Contains(c.Risk.Lock, "ACCESS EXCLUSIVE on the old index") {
		t.Fatalf("lock = %q", c.Risk.Lock)
	}
	if !containsAll(c.Targets, "public.orders", "public.orders_status_idx") {
		t.Fatalf("targets = %v", c.Targets)
	}
	if !containsAll(c.Risk.Guardrails, "approval_required") {
		t.Fatalf("guardrails = %v", c.Risk.Guardrails)
	}
	text := Text(c, now)
	for _, want := range []string{"DROP INDEX CONCURRENTLY public.orders_status_idx",
		"DROP INDEX CONCURRENTLY IF EXISTS public.orders_status_created",
		"ACCESS EXCLUSIVE on the old index"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card text lacks %q:\n%s", want, text)
		}
	}
}

func TestIndexReplaceHashCoversBothStatements(t *testing.T) {
	sql, rollback := replaceQueued()
	a := queued(9, "replace_index", sql, rollback, "moderate")
	b := a
	b.ProposedSQL = strings.Replace(sql, "orders_status_idx", "orders_other_idx", 1)
	if ContentHash(a) == ContentHash(b) {
		t.Fatal("changing the dropped index changes the approved content")
	}
	legacy := queued(10, "", sql, rollback, "moderate")
	if got := Assemble(Inputs{Action: legacy, Now: now}).ActionType; got != "replace_index" {
		t.Fatalf("an untyped row is typed from its statement pair: %q", got)
	}
}
