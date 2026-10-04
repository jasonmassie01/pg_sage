package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Dogfood lifeos, 2026-10-03: about 20 redundant-index drops in leaked
// test_* schemas spent the whole 24-hour blast radius, so two
// HypoPG-verified indexes (public.events, public.memories) were parked for
// most of a day. Hygiene now has its own budget.

// lifeosLegacyPolicyJSON is lifeos's stored standing policy (sage.policy
// id 1, ratified 2026-09-05): the legacy single blast-radius limit, which
// is read as the performance budget.
const lifeosLegacyPolicyJSON = `{"budgets": {"spend_daily": null,
 "storage_bytes": null, "llm_tokens_daily": 500000},
 "rate_limits": {"max_self_initiated_changes_per_window": 50},
 "refusal_set": ["rls_change", "grant_expansion", "major_upgrade", "non_dup_object_drop",
  "unrollbackable"],
 "blast_radius": {"max_rows_rewritten": 5000000, "max_tables_per_window": 20},
 "serialize_mode": "park", "deadline_overrides": {"xid": true, "disk": true},
 "maintenance_windows": ["always", "weekends"],
 "allowed_change_classes": ["index", "analyze", "vacuum", "freeze", "autovacuum_tuning",
  "config_guc", "retention", "fk_index", "online_migration", "backend_signal",
  "query_hint", "schema_change"],
 "unknown_classification": "fail_closed", "lock_duration_ceiling_ms": 3000,
 "approval_required_classes": ["online_migration"]}`

func lifeosLegacyPolicy() policy.Document {
	doc, err := policy.ParseDocument([]byte(lifeosLegacyPolicyJSON))
	if err != nil {
		panic(fmt.Sprintf("lifeos legacy policy: %v", err))
	}
	return doc
}

// lifeosKindTargets is the lifeos window by kind: 17 drops in leaked test
// schemas and an ANALYZE are hygiene; a covering index, two failed builds
// on memories and work_mem are performance.
func lifeosKindTargets() (hygiene, performance []string) {
	for _, target := range lifeosWindowTargets() {
		switch {
		case strings.HasPrefix(target, "test_"), target == "public.audit_log":
			hygiene = append(hygiene, target)
		default:
			performance = append(performance, target)
		}
	}
	return hygiene, performance
}

func TestHygieneDropsNoLongerStarveAVerifiedIndex(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	fx := newStaleFixtureOn(t, pool, ctx, "autonomous", verifiedDetail(), lifeosLegacyPolicy())
	hygiene, performance := lifeosKindTargets()
	spendKind(t, pool, ctx, "hygiene", 0, hygiene...)
	spendKind(t, pool, ctx, "performance", 0, performance...)

	fx.exec.RunCycle(ctx, false)

	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || !fx.indexExists(t, fx.index()) {
		t.Fatalf("actions=%d decisions=%v, want the verified index built while hygiene "+
			"is over its own budget", n, fx.decisionVerdicts(t))
	}
	var kind string
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT d.evidence->>'budget_kind',
		(d.evidence->>'rows_rewritten')::bigint FROM sage.action_log al
		JOIN sage.decision d ON d.id = al.decision_id WHERE al.sql_executed = $1`,
		fx.f.RecommendedSQL).Scan(&kind, &rows); err != nil {
		t.Fatalf("read the action's decision: %v", err)
	}
	if kind != "performance" || rows != 0 {
		t.Fatalf("executed decision: kind=%q rows=%d, want performance and 0", kind, rows)
	}
	// The hygiene budget itself is full: the next drop waits, and says why.
	drop := fx.exec.StandingPolicyGate().Authorize(ctx,
		findingRequest(dropIndexFinding("test_family_09.idx_more"), false))
	if drop.Verdict != policy.VerdictPark || drop.Reason != policy.ReasonBlastRadiusExceeded ||
		!strings.Contains(drop.Detail, "hygiene budget") {
		t.Fatalf("drop decision = %#v, want parked on the hygiene budget", drop)
	}
	assertBudgetDetailRecorded(t, pool, ctx, drop.DecisionID, "hygiene budget")
}

// A full performance budget still parks a performance change, and the
// recorded decision says which budget and when it frees.
func TestFullPerformanceBudgetParksWithItsReason(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	doc := lifeosLegacyPolicy()
	doc.BlastRadius.MaxTablesPerWindow = 2
	fx := newStaleFixtureOn(t, pool, ctx, "autonomous", verifiedDetail(), doc)
	spendKind(t, pool, ctx, "performance", 0, "public.p1", "public.p2")
	spendKind(t, pool, ctx, "hygiene", 0, "test_x.idx_1")

	fx.exec.RunCycle(ctx, false)

	if fx.indexExists(t, fx.index()) {
		t.Fatal("the index was built past the performance budget")
	}
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM sage.decision WHERE verdict = 'parked'
		AND reason = 'blast_radius_exceeded' AND target_objects = $1::jsonb`,
		`["`+fx.f.ObjectIdentifier+`"]`).Scan(&id); err != nil {
		t.Fatalf("read the parked decision: %v", err)
	}
	detail := assertBudgetDetailRecorded(t, pool, ctx, id, "performance budget")
	if !strings.Contains(detail, "3 of 2 tables") || !strings.Contains(detail, "frees at") {
		t.Fatalf("detail = %q, want usage, limit and when it frees", detail)
	}
}

func assertBudgetDetailRecorded(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, decisionID int64, want string,
) string {
	t.Helper()
	var detail string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(evidence->>'budget_detail', '')
		FROM sage.decision WHERE id = $1`, decisionID).Scan(&detail); err != nil {
		t.Fatalf("read decision %d: %v", decisionID, err)
	}
	if !strings.Contains(detail, want) {
		t.Fatalf("decision %d budget_detail = %q, want %q", decisionID, detail, want)
	}
	return detail
}

// budgetExecutor is an autonomous executor whose real standing gate reads
// doc and the isolated database's window.
func budgetExecutor(t *testing.T, pool *pgxpool.Pool, doc policy.Document) *Executor {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate = true, true
	cfg.Trust.MaintenanceWindow = "always"
	exec := New(pool, cfg, time.Now().Add(-90*24*time.Hour), func(string, string, ...any) {})
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	exec.EnableStandingPolicyDocument(doc, nil)
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.Shutdown(sctx)
	})
	return exec
}

// Candidates racing for the last table of the performance budget through
// the real gate and ledger: exactly one is authorized.
func TestLastBudgetSlotRaceAdmitsExactlyOne(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	doc := lifeosLegacyPolicy()
	doc.BlastRadius.MaxTablesPerWindow = 3
	exec := budgetExecutor(t, pool, doc)
	spendKind(t, pool, ctx, "performance", 0, "public.a", "public.b")
	gate := exec.StandingPolicyGate()

	const racers = 8
	decisions := make(chan policy.Decision, racers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < racers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			req := findingRequest(createIndexFinding(fmt.Sprintf("public.racer_%d", i)), false)
			decisions <- gate.Authorize(ctx, req)
		}(i)
	}
	start.Done()
	done.Wait()
	close(decisions)
	var winners []policy.Decision
	for decision := range decisions {
		switch decision.Verdict {
		case policy.VerdictExecute:
			winners = append(winners, decision)
		case policy.VerdictPark:
		default:
			t.Fatalf("unexpected decision %#v", decision)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d racers were authorized for the last slot, want exactly 1", len(winners))
	}
	if got := perfUsage(t, exec, ctx, "public.late"); got.TablesInWindow != 4 {
		t.Fatalf("usage while the winner runs = %+v, want its table held (4)", got)
	}
	exec.releaseBudget(ctx, winners[0].DecisionID)
	if got := perfUsage(t, exec, ctx, "public.late"); got.TablesInWindow != 3 {
		t.Fatalf("usage after release = %+v, want 3", got)
	}
}

// max_rows_rewritten is enforced on the live estimate: the window's rows
// plus the request's own may reach the limit, never pass it.
func TestMaxRowsRewrittenEnforcedOnTheEstimate(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	mustExec(t, pool, ctx, `CREATE TABLE public.rw400 (id int);
		INSERT INTO public.rw400 SELECT g FROM generate_series(1, 400) g;
		CREATE TABLE public.rw401 (id int);
		INSERT INTO public.rw401 SELECT g FROM generate_series(1, 401) g;
		ANALYZE public.rw400; ANALYZE public.rw401;`)
	spendKind(t, pool, ctx, "hygiene", 600, "public.earlier")
	doc := lifeosLegacyPolicy()
	doc.BlastRadius.MaxRowsRewritten = 1000
	gate := rewriteGate(exec, doc)

	over := gate.Authorize(ctx, vacuumFullRequest("public.rw401"))
	if over.Verdict != policy.VerdictPark || over.Reason != policy.ReasonBlastRadiusExceeded ||
		!strings.Contains(over.Detail, "rows rewritten") ||
		!strings.Contains(over.Detail, "1001 of 1000") {
		t.Fatalf("401-row rewrite = %#v, want parked at 1001 of 1000 rows", over)
	}
	atLimit := gate.Authorize(ctx, vacuumFullRequest("public.rw400"))
	if atLimit.Verdict != policy.VerdictExecute || atLimit.RowsRewritten != 400 {
		t.Fatalf("400-row rewrite = %#v, want execute at exactly the limit", atLimit)
	}
	var stamped int64
	if err := pool.QueryRow(ctx, `SELECT (evidence->>'rows_rewritten')::bigint
		FROM sage.decision WHERE id = $1`, atLimit.DecisionID).Scan(&stamped); err != nil ||
		stamped != 400 {
		t.Fatalf("recorded rows_rewritten = %d (%v), want 400", stamped, err)
	}
	// Its authorization holds the 400 rows: nothing else that rewrites fits.
	if again := gate.Authorize(ctx, vacuumFullRequest("public.rw401")); again.Verdict !=
		policy.VerdictPark {
		t.Fatalf("rewrite while 400 rows are held = %#v, want parked", again)
	}
	// A change that rewrites nothing is not limited by rows.
	analyze := gate.Authorize(ctx, policy.ActionRequest{SQL: "ANALYZE public.rw401",
		Feature: string(policy.ChangeAnalyze), TargetObjs: []string{"public.rw401"},
		Contract: &policy.ActionContract{ActionType: "analyze_table", RiskTier: policy.RiskSafe}})
	if analyze.Verdict != policy.VerdictExecute {
		t.Fatalf("ANALYZE at a full rows budget = %#v, want execute", analyze)
	}
}

func vacuumFullRequest(table string) policy.ActionRequest {
	return policy.ActionRequest{
		SQL: "VACUUM FULL " + table, Feature: string(policy.ChangeVacuum),
		TargetObjs: []string{table},
		Contract:   &policy.ActionContract{ActionType: "vacuum_table", RiskTier: policy.RiskSafe},
	}
}

// rewriteGate is the standing gate over the executor's real usage and
// ledger, with SQL validation that admits VACUUM FULL: no typed contract
// rewrites rows today, so this is the only way to exercise the limit.
func rewriteGate(exec *Executor, doc policy.Document) policy.Gate {
	decisions := ledger.NewService(ledger.NewPostgresRepository(exec.pool))
	return policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{ExecutorEnabled: true, TrustLevel: "autonomous",
				ExecutionMode: "auto", Tier3Safe: true, Tier3Moderate: true,
				RampStart: time.Now().Add(-90 * 24 * time.Hour), InConfiguredWindow: true,
			}, nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Usage: exec.standingUsage,
		RecordDecisionDetailed: func(ctx context.Context, req policy.ActionRequest,
			decision policy.Decision) (string, int64, error) {
			recorded, err := decisions.RecordDecision(ctx, ledgerInput(nil, 1, req, decision))
			return recorded.EvidenceID, recorded.ID, err
		},
	})
}

// Apply releases its authorizations when it returns, so a change that ran
// nothing (load admission withheld, a lease conflict) stops holding its
// budget slot at once instead of for the hold horizon.
func TestApplyReleasesItsAuthorizations(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	doc := lifeosLegacyPolicy()
	exec := budgetExecutor(t, pool, doc)
	req := findingRequest(createIndexFinding("public.withheld"), false)
	var seen []int64
	_, err := exec.Apply(ctx, ActionIntent{Request: req,
		Execute: func(_ context.Context, decision ActionPolicyDecision) (int64, error) {
			seen = append(seen, decision.DecisionID)
			return 0, nil // withheld after the authorization: no action row
		}})
	if err != nil || len(seen) != 1 {
		t.Fatalf("Apply = %v, executed %d times; want one execution", err, len(seen))
	}
	rows, err := pool.Query(ctx, `SELECT id, evidence ? 'budget_released' FROM sage.decision
		WHERE verdict = 'execute' AND target_objects = $1::jsonb ORDER BY id`,
		mustJSON(t, req.TargetObjs))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var released bool
		if err := rows.Scan(&id, &released); err != nil {
			t.Fatal(err)
		}
		n++
		if !released {
			t.Errorf("decision %d still holds its budget after Apply returned", id)
		}
	}
	if n != 2 {
		t.Fatalf("execute decisions = %d, want the authorization and re-authorization", n)
	}
	if got := perfUsage(t, exec, ctx, "public.other"); got.SelfInitiatedChangesInWindow != 0 {
		t.Fatalf("usage after Apply = %+v, want nothing held", got)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
