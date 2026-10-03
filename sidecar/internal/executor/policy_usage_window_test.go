package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// standingUsage reports the tables the window would hold if the request
// ran: the distinct tables of executed self-initiated actions in the last
// 24 hours plus the request's own, an index identity counted as its table.
//
// No concurrent-access test: standingUsage is one read-only query and holds
// no executor state.

func usageFor(t *testing.T, exec *Executor, ctx context.Context, targets ...string,
) policy.LimitUsage {
	t.Helper()
	usage, err := exec.standingUsage(ctx, policy.ActionRequest{TargetObjs: targets})
	if err != nil {
		t.Fatalf("standingUsage(%v): %v", targets, err)
	}
	return usage
}

func TestStandingUsageCountsTheRequestTables(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	if got := usageFor(t, exec, ctx); got != (policy.LimitUsage{}) {
		t.Fatalf("empty window, no targets: usage = %+v, want zero", got)
	}
	if got := usageFor(t, exec, ctx, "public.t|btree(a)"); got.TablesInWindow != 1 ||
		got.SelfInitiatedChangesInWindow != 0 {
		t.Fatalf("empty window, one new table: usage = %+v, want 1 table, 0 changes", got)
	}
	spendWindow(t, pool, ctx, "index", "public.t", "public.u|btree(b)")
	tests := []struct {
		name    string
		targets []string
		want    int64
	}{
		{"no targets", nil, 2},
		{"empty target", []string{""}, 2},
		{"table already counted", []string{"public.t"}, 2},
		{"identity of a counted table", []string{"public.t|btree(c)"}, 2},
		{"table counted under an identity", []string{"public.u"}, 2},
		{"new table", []string{"public.v"}, 3},
		{"new table twice", []string{"public.v", "public.v|btree(x)"}, 3},
		{"two new tables", []string{"public.v", "other.w"}, 4},
	}
	for _, tt := range tests {
		got := usageFor(t, exec, ctx, tt.targets...)
		if got.TablesInWindow != tt.want || got.SelfInitiatedChangesInWindow != 2 {
			t.Errorf("%s: usage = %+v, want %d tables and 2 changes", tt.name, got, tt.want)
		}
	}
}

// Operator-approved actions are not self-initiated, and the window is the
// last 24 hours: an action just inside counts, one just outside does not.
func TestStandingUsageWindowEdgesAndOperatorActions(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	spendWindow(t, pool, ctx, operatorDecisionIntent, "public.operator_table")
	inside := spendWindow(t, pool, ctx, "index", "public.inside")
	outside := spendWindow(t, pool, ctx, "index", "public.outside")
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET executed_at =
		now() - interval '23 hours 59 minutes' WHERE id = $1`, inside[0]); err != nil {
		t.Fatal(err)
	}
	ageOut(t, pool, ctx, outside[0])

	got := usageFor(t, exec, ctx)
	if got.TablesInWindow != 1 || got.SelfInitiatedChangesInWindow != 1 {
		t.Fatalf("usage = %+v, want only public.inside: 1 table, 1 change", got)
	}
	// The operator's table is not in the self-initiated window, so an
	// autonomous change on it adds a table.
	if got := usageFor(t, exec, ctx, "public.operator_table"); got.TablesInWindow != 2 {
		t.Fatalf("usage = %+v, want 2 tables with the operator's table", got)
	}
}

// A decision on several targets counts each of its tables once.
func TestStandingUsageMultiTargetDecision(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	ids := spendWindow(t, pool, ctx, "index", "public.a")
	if _, err := pool.Exec(ctx, `UPDATE sage.decision SET target_objects =
		'["public.a", "public.b|btree(x)", "public.b"]'::jsonb WHERE id =
		(SELECT decision_id FROM sage.action_log WHERE id = $1)`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got := usageFor(t, exec, ctx, "public.b|btree(y)"); got.TablesInWindow != 2 ||
		got.SelfInitiatedChangesInWindow != 1 {
		t.Fatalf("usage = %+v, want 2 tables and 1 change", got)
	}
}

// Errors are distinguishable: no pool versus a failed read.
func TestStandingUsageErrors(t *testing.T) {
	exec := New(nil, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	_, err := exec.standingUsage(context.Background(), policy.ActionRequest{})
	if err == nil || !strings.Contains(err.Error(), "requires a database pool") {
		t.Fatalf("nil pool: err = %v, want a missing-pool error", err)
	}
	pool, ctx := isolatedSageDB(t)
	exec = New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	if _, err := pool.Exec(ctx, "DROP TABLE sage.action_log CASCADE"); err != nil {
		t.Fatal(err)
	}
	_, err = exec.standingUsage(ctx, policy.ActionRequest{TargetObjs: []string{"public.t"}})
	if err == nil || !strings.Contains(err.Error(), "read policy usage") {
		t.Fatalf("missing table: err = %v, want a read policy usage error", err)
	}
}
