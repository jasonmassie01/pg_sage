package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// standingUsage charges each change to its budget kind, recorded on the
// authorizing decision as evidence.budget_kind. Decisions without a kind
// (recorded before the split) or with an unknown one charge performance:
// fail closed. Rows rewritten are one budget shared by every kind.

// spendKind records one executed self-initiated action per target whose
// decision carries the budget kind and rows rewritten, and returns the
// action_log ids.
func spendKind(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, kind string, rows int64,
	targets ...string,
) []int64 {
	t.Helper()
	decisions := ledger.NewService(ledger.NewPostgresRepository(pool))
	ids := make([]int64, 0, len(targets))
	for _, target := range targets {
		decision, err := decisions.RecordDecision(ctx, ledger.DecisionInput{
			Feature: "index", Intent: "index", Verdict: ledger.VerdictExecute,
			Reason: "authorized", RiskTier: "moderate", PolicyVersion: 1,
			TargetObjects: []string{target},
			Evidence:      map[string]any{"budget_kind": kind, "rows_rewritten": rows},
		})
		if err != nil {
			t.Fatalf("record decision on %s: %v", target, err)
		}
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
			(action_type, sql_executed, outcome, decision_id)
			VALUES ('drop_index', $1, 'success', $2) RETURNING id`,
			"DROP INDEX CONCURRENTLY "+target, decision.ID).Scan(&id); err != nil {
			t.Fatalf("insert action on %s: %v", target, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func createIndexFinding(table string) analyzer.Finding {
	return analyzer.Finding{
		Category: "missing_index", ObjectType: "index",
		ObjectIdentifier: table + "|btree(a)",
		RecommendedSQL:   "CREATE INDEX CONCURRENTLY idx_budget ON " + table + " (a)",
		Detail:           verifiedDetail(),
	}
}

func dropIndexFinding(index string) analyzer.Finding {
	return analyzer.Finding{
		Category: "duplicate_index", ObjectType: "index", ObjectIdentifier: index,
		RecommendedSQL: "DROP INDEX CONCURRENTLY " + index,
	}
}

func perfUsage(t *testing.T, exec *Executor, ctx context.Context, table string,
) policy.LimitUsage {
	t.Helper()
	return usageOf(t, exec, ctx, findingRequest(createIndexFinding(table), false))
}

func hygieneUsage(t *testing.T, exec *Executor, ctx context.Context, index string,
) policy.LimitUsage {
	t.Helper()
	return usageOf(t, exec, ctx, findingRequest(dropIndexFinding(index), false))
}

func usageOf(t *testing.T, exec *Executor, ctx context.Context, req policy.ActionRequest,
) policy.LimitUsage {
	t.Helper()
	usage, err := exec.standingUsage(ctx, req)
	if err != nil {
		t.Fatalf("standingUsage(%s): %v", req.SQL, err)
	}
	return usage
}

func TestStandingUsageChargesEachKindSeparately(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	spendKind(t, pool, ctx, "hygiene", 0, "test_a.idx_1", "test_a.idx_2", "test_b.idx_3")
	spendKind(t, pool, ctx, "performance", 0, "public.events|btree(a)")
	spendWindow(t, pool, ctx, "index", "public.legacy") // no kind: performance
	spendKind(t, pool, ctx, "maintenance", 0, "public.bogus") // unknown: performance

	perf := perfUsage(t, exec, ctx, "public.memories")
	if perf.TablesInWindow != 4 || perf.SelfInitiatedChangesInWindow != 3 {
		t.Fatalf("performance usage = %+v, want 4 tables (events, legacy, bogus, "+
			"memories) and 3 changes", perf)
	}
	hygiene := hygieneUsage(t, exec, ctx, "test_c.idx_4")
	if hygiene.TablesInWindow != 4 || hygiene.SelfInitiatedChangesInWindow != 3 {
		t.Fatalf("hygiene usage = %+v, want 4 tables and 3 changes", hygiene)
	}
	// A table counted by the other kind is new to this one.
	if got := hygieneUsage(t, exec, ctx, "public.events"); got.TablesInWindow != 4 {
		t.Fatalf("hygiene on a performance table: usage = %+v, want 4 tables", got)
	}
}

// Rows rewritten are summed over the window across both kinds, plus the
// request's own estimate; an action aged out of the window frees its rows.
func TestStandingUsageSumsRowsRewrittenAcrossKinds(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	mustExec(t, pool, ctx, `CREATE TABLE public.rw_win (id int);
		INSERT INTO public.rw_win SELECT g FROM generate_series(1, 70) g;
		ANALYZE public.rw_win;`)
	hygiene := spendKind(t, pool, ctx, "hygiene", 300, "public.a")
	spendKind(t, pool, ctx, "performance", 200, "public.b")
	old := spendKind(t, pool, ctx, "performance", 1000, "public.c")
	ageOut(t, pool, ctx, old[0])
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET executed_at =
		now() - interval '23 hours' WHERE id = $1`, hygiene[0]); err != nil {
		t.Fatal(err)
	}

	got := perfUsage(t, exec, ctx, "public.d")
	if got.RowsRewritten != 500 || got.RequestRowsRewritten != 0 {
		t.Fatalf("usage = %+v, want 500 rows in the window and 0 for the request", got)
	}
	wantFree := time.Now().Add(time.Hour)
	if got.RowsFreeAt.Before(wantFree.Add(-2*time.Minute)) ||
		got.RowsFreeAt.After(wantFree.Add(2*time.Minute)) {
		t.Fatalf("rows free at %v, want about %v (the oldest rewrite ages out)",
			got.RowsFreeAt, wantFree)
	}
	rewrite := policy.ActionRequest{SQL: "VACUUM FULL public.rw_win",
		TargetObjs: []string{"public.rw_win"},
		Contract:   &policy.ActionContract{ActionType: "vacuum_table", RiskTier: policy.RiskSafe}}
	got = usageOf(t, exec, ctx, rewrite)
	if got.RowsRewritten != 570 || got.RequestRowsRewritten != 70 {
		t.Fatalf("rewrite usage = %+v, want 570 rows of which 70 are the request's", got)
	}
}

// The window reports when it next frees room for each limit: the earliest
// counted change or table plus 24 hours.
func TestStandingUsageReportsWhenEachLimitFrees(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	if got := perfUsage(t, exec, ctx, "public.x"); !got.TablesFreeAt.IsZero() ||
		!got.ChangesFreeAt.IsZero() || !got.RowsFreeAt.IsZero() {
		t.Fatalf("empty window: usage = %+v, want no free times", got)
	}
	ids := spendKind(t, pool, ctx, "performance", 0, "public.a", "public.b")
	// public.a was touched 22 hours ago and again 1 hour ago: it frees with
	// its last touch. public.b, touched once 20 hours ago, frees first.
	again := spendKind(t, pool, ctx, "performance", 0, "public.a")
	for id, age := range map[int64]string{ids[0]: "22 hours", ids[1]: "20 hours",
		again[0]: "1 hour"} {
		if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET executed_at =
			now() - $2::interval WHERE id = $1`, id, age); err != nil {
			t.Fatal(err)
		}
	}
	got := perfUsage(t, exec, ctx, "public.x")
	assertAbout(t, "changes free at", got.ChangesFreeAt, 2*time.Hour)
	assertAbout(t, "tables free at", got.TablesFreeAt, 4*time.Hour)
	if got.TablesInWindow != 3 || got.SelfInitiatedChangesInWindow != 3 {
		t.Fatalf("usage = %+v, want 3 tables and 3 changes", got)
	}
}

func assertAbout(t *testing.T, name string, got time.Time, fromNow time.Duration) {
	t.Helper()
	want := time.Now().Add(fromNow)
	if got.Before(want.Add(-2*time.Minute)) || got.After(want.Add(2*time.Minute)) {
		t.Fatalf("%s = %v, want about %v", name, got, want)
	}
}

// recordExecute records an execute decision for req exactly as the
// standing gate does (an authorization whose action has not run yet).
func recordExecute(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, req policy.ActionRequest,
	rows int64,
) int64 {
	t.Helper()
	decision := policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonAuthorized, RiskTier: policy.RiskModerate,
		BudgetKind: policy.BudgetKindFor(req), RowsRewritten: rows}
	recorded, err := ledger.NewService(ledger.NewPostgresRepository(pool)).
		RecordDecision(ctx, ledgerInput(nil, 1, req, decision))
	if err != nil {
		t.Fatalf("record execute decision: %v", err)
	}
	return recorded.ID
}

// An authorized change whose action has not run yet holds its budget
// slot, so a concurrent candidate cannot take it too. The same request
// re-authorizing does not count itself; a released, expired or executed
// authorization is not held twice.
func TestStandingUsageHoldsInFlightAuthorizations(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	inflight := findingRequest(createIndexFinding("public.inflight"), false)
	first := recordExecute(t, pool, ctx, inflight, 25)
	second := recordExecute(t, pool, ctx, inflight, 25) // its re-authorization

	got := perfUsage(t, exec, ctx, "public.other")
	if got.TablesInWindow != 2 || got.SelfInitiatedChangesInWindow != 1 ||
		got.RowsRewritten != 25 {
		t.Fatalf("other request: usage = %+v, want the in-flight change counted once "+
			"(2 tables, 1 change, 25 rows)", got)
	}
	if got := usageOf(t, exec, ctx, inflight); got.TablesInWindow != 1 ||
		got.SelfInitiatedChangesInWindow != 0 || got.RowsRewritten != 0 {
		t.Fatalf("re-authorization: usage = %+v, want only its own table", got)
	}
	if got := hygieneUsage(t, exec, ctx, "public.idx"); got.SelfInitiatedChangesInWindow != 0 ||
		got.RowsRewritten != 25 {
		t.Fatalf("hygiene: usage = %+v, want no hygiene change and the shared 25 rows", got)
	}
	exec.releaseBudget(ctx, first, second)
	if got := perfUsage(t, exec, ctx, "public.other"); got.SelfInitiatedChangesInWindow != 0 ||
		got.TablesInWindow != 1 {
		t.Fatalf("after release: usage = %+v, want nothing held", got)
	}

	expired := recordExecute(t, pool, ctx, inflight, 0)
	if _, err := pool.Exec(ctx, `UPDATE sage.decision
		SET created_at = now() - make_interval(secs => $2) WHERE id = $1`,
		expired, (exec.budgetHoldHorizon() + time.Minute).Seconds()); err != nil {
		t.Fatal(err)
	}
	if got := perfUsage(t, exec, ctx, "public.other"); got.SelfInitiatedChangesInWindow != 0 {
		t.Fatalf("expired hold: usage = %+v, want nothing held", got)
	}

	// The first authorization stays unreleased until Apply returns, while
	// the re-authorization that followed it already backs the action.
	pending := recordExecute(t, pool, ctx, inflight, 0)
	executed := recordExecute(t, pool, ctx, inflight, 0)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome, decision_id) VALUES ('create_index', $1, 'success', $2)`,
		inflight.SQL, executed); err != nil {
		t.Fatal(err)
	}
	if got := perfUsage(t, exec, ctx, "public.other"); got.SelfInitiatedChangesInWindow != 1 {
		t.Fatalf("executed change (first authorization %d pending): usage = %+v, "+
			"want it counted once", pending, got)
	}
}

// A request's evidence cannot claim a budget kind, rows or a release: the
// ledger stamps them from the typed contract and the gate's estimate.
func TestLedgerInputStampsBudgetAndIgnoresSpoofedEvidence(t *testing.T) {
	spoofed := map[string]any{"budget_kind": "hygiene", "rows_rewritten": 0,
		"budget_released": true, "budget_detail": "fake"}
	perf := findingRequest(createIndexFinding("public.t"), false)
	perf.Evidence = spoofed
	input := ledgerInput(nil, 1, perf, policy.Decision{Verdict: policy.VerdictExecute,
		BudgetKind: policy.BudgetPerformance, RowsRewritten: 500})
	if input.Evidence["budget_kind"] != "performance" ||
		input.Evidence["rows_rewritten"] != int64(500) {
		t.Fatalf("evidence = %v, want kind performance and 500 rows", input.Evidence)
	}
	if _, ok := input.Evidence["budget_released"]; ok {
		t.Fatalf("evidence = %v, a request must not release its own hold", input.Evidence)
	}
	if _, ok := input.Evidence["budget_detail"]; ok {
		t.Fatalf("evidence = %v, want no budget detail on an execute", input.Evidence)
	}
	drop := findingRequest(dropIndexFinding("public.idx"), false)
	parked := ledgerInput(nil, 1, drop, policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonBlastRadiusExceeded, BudgetKind: policy.BudgetHygiene,
		Detail: "hygiene budget full: 11 of 10 tables"})
	if parked.Evidence["budget_kind"] != "hygiene" ||
		parked.Evidence["budget_detail"] != "hygiene budget full: 11 of 10 tables" {
		t.Fatalf("parked evidence = %v, want kind hygiene and the budget detail",
			parked.Evidence)
	}
	bare := ledgerInput(nil, 1, policy.ActionRequest{SQL: "ANALYZE public.t"},
		policy.Decision{Verdict: policy.VerdictBlocked, Reason: policy.ReasonNoTypedContract})
	if bare.Evidence["budget_kind"] != "performance" {
		t.Fatalf("no contract: evidence = %v, want performance (fail closed)", bare.Evidence)
	}
}

// Errors stay distinguishable: a failed rows estimate names the estimate.
func TestStandingUsageRowsEstimateError(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	_, err := exec.standingUsage(ctx, policy.ActionRequest{SQL: "VACUUM FULL public.gone",
		Contract: &policy.ActionContract{ActionType: "vacuum_table", RiskTier: policy.RiskSafe}})
	if err == nil || !strings.Contains(err.Error(), "rows rewritten") {
		t.Fatalf("err = %v, want a rows-rewritten estimate error", err)
	}
}

// Post-test audit: decisions the ledger stored without targets (a nil
// slice is the JSON null) or for read-only diagnostics must neither break
// the window read nor hold a mutation slot.
func TestStandingUsageToleratesNullTargetsAndSkipsReadOnly(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	decisions := ledger.NewService(ledger.NewPostgresRepository(pool))
	for tier, sql := range map[string]string{
		"moderate":  "ALTER SYSTEM SET work_mem = '8MB'",
		"read_only": "SELECT pid FROM pg_locks WHERE NOT granted",
	} {
		if _, err := decisions.RecordDecision(ctx, ledger.DecisionInput{
			Feature: "config_guc", Intent: "config_guc", Verdict: ledger.VerdictExecute,
			Reason: "authorized", RiskTier: tier, PolicyVersion: 1,
			ProposedSQL: sql,
		}); err != nil {
			t.Fatalf("record %s decision without targets: %v", tier, err)
		}
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT target_objects::text FROM sage.decision
		WHERE risk_tier = 'moderate'`).Scan(&stored); err != nil || stored != "null" {
		t.Fatalf("stored targets = %q (%v), want the JSON null this test guards", stored, err)
	}
	got := perfUsage(t, exec, ctx, "public.t")
	if got.SelfInitiatedChangesInWindow != 1 || got.TablesInWindow != 1 {
		t.Fatalf("usage = %+v, want the moderate change held (1) and only the request's "+
			"table; the read-only diagnostic holds nothing", got)
	}
	// The same target-less request re-authorizing does not count itself.
	self := policy.ActionRequest{SQL: "ALTER SYSTEM SET work_mem = '8MB'",
		Contract: &policy.ActionContract{ActionType: "alter_system_guc",
			RiskTier: policy.RiskModerate}}
	if got := usageOf(t, exec, ctx, self); got.SelfInitiatedChangesInWindow != 0 {
		t.Fatalf("target-less re-authorization: usage = %+v, want 0 changes", got)
	}
}
