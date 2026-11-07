package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// vacuum-01 integration (real PostgreSQL): before the standing gate, a
// VACUUM finding's target is resolved with to_regclass on the statement's
// own (quoted) target. A partitioned parent needs an operator: VACUUM
// recurses into every partition. A leaf, a mixed-case table and a table
// whose name contains a dot run as before; a dropped table is a stale
// finding, skipped and never queued.

const guardSchema = "vac_guard"

func createGuardFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+guardSchema+" CASCADE")
		_, _ = pool.Exec(ctx, `DROP SCHEMA IF EXISTS "VacGuard" CASCADE`)
	})
	for _, sql := range []string{
		"DROP SCHEMA IF EXISTS " + guardSchema + " CASCADE",
		`DROP SCHEMA IF EXISTS "VacGuard" CASCADE`,
		"CREATE SCHEMA " + guardSchema,
		`CREATE SCHEMA "VacGuard"`,
		"CREATE TABLE vac_guard.parent (id int, k int) PARTITION BY RANGE (k)",
		`CREATE TABLE vac_guard.leaf PARTITION OF vac_guard.parent
		   FOR VALUES FROM (0) TO (10)`,
		`CREATE TABLE vac_guard.leaf2 PARTITION OF vac_guard.parent
		   FOR VALUES FROM (10) TO (20)`,
		"INSERT INTO vac_guard.parent SELECT g, g % 20 FROM generate_series(1, 200) g",
		`CREATE TABLE "VacGuard"."MyTable" (id int)`,
		`CREATE TABLE vac_guard."my.table" (id int)`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func TestVacuumParentGuard_ResolvesTheStatementTarget(t *testing.T) {
	pool, ctx := requireDB(t)
	createGuardFixture(t, ctx, pool)
	log := &guardLog{}
	exec := New(pool, &config.Config{}, time.Time{}, log.logf)
	for _, tc := range []struct {
		sql      string
		approval bool
	}{
		{`VACUUM (FREEZE) "vac_guard"."parent";`, true},
		{"VACUUM vac_guard.parent", true},
		{"VACUUM (ANALYZE) vac_guard.parent (id)", true},
		{`VACUUM (FREEZE) "vac_guard"."leaf";`, false},
		{`VACUUM (FREEZE) "VacGuard"."MyTable";`, false},
		{`VACUUM (FREEZE) "vac_guard"."my.table";`, false},
	} {
		got, stale := exec.guardVacuumTarget(ctx, vacuumFinding(tc.sql))
		reason := approvalRequiredReason(got)
		if stale || (reason != "") != tc.approval {
			t.Errorf("%q: stale=%v reason=%q, want approval=%v", tc.sql, stale, reason,
				tc.approval)
		}
		if tc.approval && !strings.Contains(reason, "partitioned") {
			t.Errorf("%q: reason %q does not say why", tc.sql, reason)
		}
	}
}

func TestVacuumParentGuard_DroppedTableIsStale(t *testing.T) {
	pool, ctx := requireDB(t)
	log := &guardLog{}
	exec := New(pool, &config.Config{}, time.Time{}, log.logf)
	missing := fmt.Sprintf(`VACUUM (FREEZE) "public"."vac_gone_%d";`, time.Now().UnixNano())
	got, stale := exec.guardVacuumTarget(ctx, vacuumFinding(missing))
	if !stale || approvalRequiredReason(got) != "" {
		t.Fatalf("dropped target: stale=%v reason=%q, want stale and no approval",
			stale, approvalRequiredReason(got))
	}
	if !log.contains("no longer exists") {
		t.Fatalf("log = %v, want the stale skip recorded", log.lines)
	}
}

// Error propagation: a failed relkind lookup (an improper name the server
// rejects, a cancelled context) queues the VACUUM for approval and logs
// the target; it is never treated as "not partitioned".
func TestVacuumParentGuard_LookupErrorRequiresApproval(t *testing.T) {
	pool, ctx := requireDB(t)
	log := &guardLog{}
	exec := New(pool, &config.Config{}, time.Time{}, log.logf)
	improper := vacuumFinding("VACUUM a.b.c.d")
	got, stale := exec.guardVacuumTarget(ctx, improper)
	if stale || !strings.Contains(approvalRequiredReason(got), "a.b.c.d") {
		t.Fatalf("improper name: stale=%v reason=%q", stale, approvalRequiredReason(got))
	}
	if !log.contains("a.b.c.d") {
		t.Fatalf("log = %v, want the target named", log.lines)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, stale = exec.guardVacuumTarget(cancelled, vacuumFinding("VACUUM public.t"))
	if stale || approvalRequiredReason(got) == "" {
		t.Fatalf("cancelled lookup: stale=%v reason=%q", stale, approvalRequiredReason(got))
	}
}

// CHECK-03 through the background path: with freeze trusted (grandfathered
// L3), a leaf's VACUUM (FREEZE) runs, the parent's is queued for approval
// and never executed, and a dropped table's finding is neither.
func TestVacuumParentGuard_ProcessFindingQueuesParentOnly(t *testing.T) {
	r := newShadowRig(t, true)
	createGuardFixture(t, r.ctx, r.pool)
	parent := r.freezeFinding("vac_guard.parent", `VACUUM (FREEZE) "vac_guard"."parent";`)
	leaf := r.freezeFinding("vac_guard.leaf", `VACUUM (FREEZE) "vac_guard"."leaf";`)
	gone := r.freezeFinding("vac_guard.gone", `VACUUM (FREEZE) "vac_guard"."gone";`)
	for _, f := range []analyzer.Finding{parent, leaf, gone} {
		r.exec.processFinding(r.ctx, f, false, nil)
	}
	if n := r.actions(parent.RecommendedSQL); n != 0 {
		t.Fatalf("the parent VACUUM executed (%d action_log rows)", n)
	}
	if n := r.queued(parent.RecommendedSQL); n != 1 {
		t.Fatalf("parent VACUUM pending approvals = %d, want 1", n)
	}
	if n := r.actions(leaf.RecommendedSQL); n != 1 {
		t.Fatalf("leaf VACUUM action_log rows = %d, want 1 (it runs unattended)", n)
	}
	if n := r.queued(leaf.RecommendedSQL); n != 0 {
		t.Fatalf("leaf VACUUM pending approvals = %d, want 0", n)
	}
	if r.actions(gone.RecommendedSQL) != 0 || r.queued(gone.RecommendedSQL) != 0 {
		t.Fatal("a dropped table's VACUUM was executed or queued")
	}
}

// freezeFinding opens a wraparound finding for object with sql.
func (r *shadowRig) freezeFinding(object, sql string) analyzer.Finding {
	r.t.Helper()
	f := analyzer.Finding{Category: "wraparound_freeze", Severity: "critical",
		ObjectType: "table", ObjectIdentifier: object, Title: "freeze " + object,
		Recommendation: "freeze it", RecommendedSQL: sql, ActionRisk: "safe",
		Detail: map[string]any{"xid_age": 210_000_000}}
	r.openFinding(f)
	r.t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			"DELETE FROM sage.shadow_decision WHERE object_identifier = $1",
			"DELETE FROM sage.findings WHERE object_identifier = $1",
		} {
			_, _ = r.pool.Exec(ctx, q, object)
		}
		_, _ = r.pool.Exec(ctx, "DELETE FROM sage.action_queue WHERE proposed_sql = $1", sql)
	})
	return f
}

func (r *shadowRig) queued(sql string) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM sage.action_queue
		WHERE proposed_sql = $1 AND status = 'pending'`, sql).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}
