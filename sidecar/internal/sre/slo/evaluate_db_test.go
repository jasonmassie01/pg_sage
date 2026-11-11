package slo

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// playbooks-09: budget_coverage and budget_spent_so_far live in the
// stored status document, so the SLOs list and detail read them back
// after a restart; a later known budget clears the spent-so-far.
func TestStore_BudgetCoverageRoundTrip(t *testing.T) {
	st, scope, ctx := liveStore(t)
	o := appObjective()
	o.Name = uniqueName("budget")
	partial := Evaluate(o, DefaultRules(), windowsAt(allBurning(0.1)),
		combined(o, 3*24*time.Hour, 50, 100000), now)
	if _, _, err := st.Save(ctx, scope, partial); err != nil {
		t.Fatalf("save partial: %v", err)
	}
	got := onlyStatus(t, st, scope, ctx)
	if got.BudgetUnknown != ReasonPartialWindow || got.BudgetRemaining != nil {
		t.Fatalf("stored budget = %v / %q", got.BudgetRemaining, got.BudgetUnknown)
	}
	wantFloat(t, "stored budget_coverage", got.BudgetCoverage, 0.1)
	wantFloat(t, "stored budget_spent_so_far", got.BudgetSpentSoFar, 0.5)

	full := Evaluate(o, DefaultRules(), windowsAt(allBurning(0.1)),
		combined(o, budgetWindow, 25, 100000), now.Add(time.Minute))
	if _, _, err := st.Save(ctx, scope, full); err != nil {
		t.Fatalf("save full: %v", err)
	}
	got = onlyStatus(t, st, scope, ctx)
	wantFloat(t, "stored budget_remaining", got.BudgetRemaining, 0.75)
	wantFloat(t, "stored budget_coverage", got.BudgetCoverage, 1)
	if got.BudgetSpentSoFar != nil || got.BudgetUnknown != "" {
		t.Fatalf("a known budget kept spent %v / unknown %q", got.BudgetSpentSoFar,
			got.BudgetUnknown)
	}
}

// onlyStatus reads the one status a fresh scope holds.
func onlyStatus(t *testing.T, st *Store, scope sre.Scope, ctx context.Context) Status {
	t.Helper()
	got, err := st.Statuses(ctx, scope)
	if err != nil || len(got) != 1 {
		t.Fatalf("statuses = %+v err=%v", got, err)
	}
	return got[0]
}
