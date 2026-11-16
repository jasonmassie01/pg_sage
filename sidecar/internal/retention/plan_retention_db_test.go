package retention

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// plan-01a: a plan in a terminal status ages out with retention.actions_days
// on updated_at (its steps, edges and approvals cascade); an open plan is
// kept however old. A workload pack that an open plan, or a terminal plan
// younger than actions_days, cites survives retention.workload_packs_days.
// No concurrency test: one cleaner runs per database.

func planRetentionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "plan_retention"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func planRetentionCfg() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Retention.ActionsDays = 365
	cfg.Retention.WorkloadPacksDays = 180
	return cfg
}

// insertPlan adds a plan with one step and an approval, last updated age
// ago, citing pack (when not "").
func insertPlan(t *testing.T, pool *pgxpool.Pool, status, pack string,
	age time.Duration) int64 {
	t.Helper()
	ctx := context.Background()
	var packArg any
	if pack != "" {
		packArg = pack
	}
	var id int64
	at := time.Now().Add(-age)
	if err := pool.QueryRow(ctx, `INSERT INTO sage.plan (goal, origin, pack_hash, status,
		plan_hash, created_at, updated_at, created_by)
		VALUES ('g', 'person', $1, $2, repeat('a', 64), $3, $3, 'alice') RETURNING id`,
		packArg, status, at).Scan(&id); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.plan_step (plan_id, ordinal, kind, metric,
		expected_direction, expected_source, deadline_minutes, on_improved, on_no_change,
		on_regressed, on_inconclusive, on_expired)
		VALUES ($1, 1, 'check', 'readback', 'none', 'none', 10, 'keep', 'keep',
		        'revert_step', 'halt_ask', 'halt_ask')`, id); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.plan_approval (plan_id, plan_hash,
		approver_user_id, decided_at) VALUES ($1, repeat('a', 64), 7, $2)`, id, at); err != nil {
		t.Fatalf("insert approval: %v", err)
	}
	return id
}

func insertPlanPack(t *testing.T, pool *pgxpool.Pool, n int, age time.Duration) string {
	t.Helper()
	sha := fmt.Sprintf("%064x", n)
	at := time.Now().Add(-age)
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.workload_packs
		(window_from, window_to, format, content_sha256, tier, bytes, built_at, built_by,
		 manifest, body)
		VALUES ($1, $2, 'pg_sage-workload/1', $3, 'local', 1, $1, 't', '{}', '\x00')`,
		at, at.Add(time.Hour), sha); err != nil {
		t.Fatalf("insert pack: %v", err)
	}
	return sha
}

func planCount(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestRetention_TerminalPlansAgeOutOpenPlansStay(t *testing.T) {
	pool := planRetentionPool(t)
	old := 400 * 24 * time.Hour
	gone := insertPlan(t, pool, "completed", "", old)
	recent := insertPlan(t, pool, "voided", "", 300*24*time.Hour)
	running := insertPlan(t, pool, "running", "", old)
	halted := insertPlan(t, pool, "halted", "", old)
	proposed := insertPlan(t, pool, "proposed", "", old)
	New(pool, planRetentionCfg(), noopLog).RunOnce(context.Background())
	for id, want := range map[int64]int{gone: 0, recent: 1, running: 1, halted: 1, proposed: 1} {
		if n := planCount(t, pool, `SELECT count(*) FROM sage.plan WHERE id = $1`, id); n != want {
			t.Errorf("plan %d: %d rows, want %d", id, n, want)
		}
	}
	if n := planCount(t, pool, `SELECT count(*) FROM sage.plan_step WHERE plan_id = $1`,
		gone); n != 0 {
		t.Fatalf("steps of a purged plan: %d, want 0 (cascade)", n)
	}
	if n := planCount(t, pool, `SELECT count(*) FROM sage.plan_approval WHERE plan_id = $1`,
		gone); n != 0 {
		t.Fatalf("approvals of a purged plan: %d, want 0 (cascade)", n)
	}
}

func TestRetention_CitedPacksOutliveTheirWindow(t *testing.T) {
	pool := planRetentionPool(t)
	old := 200 * 24 * time.Hour
	cited := insertPlanPack(t, pool, 1, old)
	citedTerminal := insertPlanPack(t, pool, 2, old)
	staleCite := insertPlanPack(t, pool, 3, old)
	uncited := insertPlanPack(t, pool, 4, old)
	young := insertPlanPack(t, pool, 5, time.Hour)
	insertPlan(t, pool, "approved", cited, old)
	insertPlan(t, pool, "completed", citedTerminal, 100*24*time.Hour)
	// A terminal plan older than actions_days no longer protects its pack
	// (that plan itself is purged in the same run).
	insertPlan(t, pool, "completed", staleCite, 400*24*time.Hour)
	New(pool, planRetentionCfg(), noopLog).RunOnce(context.Background())
	for sha, want := range map[string]int{cited: 1, citedTerminal: 1, staleCite: 0,
		uncited: 0, young: 1} {
		if n := planCount(t, pool, `SELECT count(*) FROM sage.workload_packs
			WHERE content_sha256 = $1`, sha); n != want {
			t.Errorf("pack %s: %d rows, want %d", sha[60:], n, want)
		}
	}
}

func TestPurgeRules_PlanRuleAndPackKeep(t *testing.T) {
	cfg := planRetentionCfg()
	var plan, packs []purgeRule
	for _, r := range purgeRules(cfg) {
		switch r.table {
		case "plan":
			plan = append(plan, r)
		case "workload_packs":
			packs = append(packs, r)
		}
	}
	if len(plan) != 1 || plan[0].timeCol != "updated_at" || plan[0].days != 365 ||
		!strings.Contains(plan[0].extra, "'completed'") ||
		strings.Contains(plan[0].extra, "'running'") ||
		strings.Contains(plan[0].extra, "'halted'") {
		t.Fatalf("plan rules %+v, want one on updated_at for actions_days, terminal only", plan)
	}
	if len(packs) != 1 || !strings.Contains(packs[0].extra, "sage.plan") ||
		!strings.Contains(packs[0].extra, "make_interval(days => 365)") {
		t.Fatalf("workload_packs rules %+v, want the plan keep predicate", packs)
	}
	cfg.Retention.ActionsDays = 0 // actions kept forever: every citing plan keeps its pack
	for _, r := range purgeRules(cfg) {
		if r.table == "workload_packs" && strings.Contains(r.extra, "make_interval") {
			t.Fatalf("actions_days 0 still ages out cited packs: %s", r.extra)
		}
	}
}
