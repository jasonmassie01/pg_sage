package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// fixedGate returns one verdict for every authorization and previews
// operator actions as executable.
type fixedGate struct {
	verdict    policy.Verdict
	decisionID int64
}

func (g fixedGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	return policy.Decision{Verdict: g.verdict, RiskTier: policy.RiskSafe,
		DecisionID: g.decisionID}
}

func (g fixedGate) Explain(context.Context, policy.ActionRequest) policy.Decision {
	return policy.Decision{Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe}
}

type recFixture struct {
	pool  *pgxpool.Pool
	ctx   context.Context
	table string
	sql   string
	f     analyzer.Finding
	id    int64 // finding id
	exec  *Executor
	recs  *recommendation.Store
}

// newRecFixture persists a finding for sql (with {table} replaced) and
// builds an executor whose analyzer memory is empty: every candidate
// must come from the durable recommendation state (C07).
func newRecFixture(t *testing.T, sqlTemplate string, verdict policy.Verdict) *recFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	table := probeTable(t, pool, "rec_cycle")
	sql := strings.ReplaceAll(sqlTemplate, "{table}", table)
	fx := &recFixture{pool: pool, ctx: ctx, table: table, sql: sql,
		recs: recommendation.NewStore(pool)}
	fx.f = analyzer.Finding{
		Category: "rec_cycle", ObjectIdentifier: "public." + table, ObjectType: "table",
		Title: "rec cycle " + table, Severity: "warning", RecommendedSQL: sql,
		ActionRisk: "safe",
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql)
		VALUES ($1, 'warning', 'table', $2, $3, '{}', 'rec', $4) RETURNING id`,
		fx.f.Category, fx.f.ObjectIdentifier, fx.f.Title, sql).Scan(&fx.id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	fx.exec = New(pool, cfg, time.Now().Add(-90*24*time.Hour), nopLog)
	fx.exec.emergencyStopFn = func(context.Context) bool { return false }
	fx.exec.WithDatabaseName("rec_" + table)
	fx.exec.WithPolicyGate(fixedGate{verdict: verdict,
		decisionID: recordCustodianDecision(t, ctx, pool, "rec_cycle", table)})
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = fx.exec.Shutdown(sctx)
		closeTestMonitors(t, pool, sql)
	})
	return fx
}

func (fx *recFixture) propose(t *testing.T) recommendation.Recommendation {
	t.Helper()
	res, err := fx.recs.Propose(fx.ctx,
		analyzer.RecommendationProposal(fx.exec.databaseName, fx.f))
	if err != nil || res.Outcome != recommendation.OutcomeCreated {
		t.Fatalf("propose: %+v, %v", res, err)
	}
	return res.Recommendation
}

func (fx *recFixture) get(t *testing.T, id int64) recommendation.Recommendation {
	t.Helper()
	rec, err := fx.recs.Get(fx.ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return rec
}

func (fx *recFixture) actionRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, fx.sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (fx *recFixture) probeApplied(t *testing.T) bool {
	t.Helper()
	var options []string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT COALESCE(reloptions, '{}')
		FROM pg_class WHERE oid = to_regclass($1)`, "public."+fx.table).Scan(&options); err != nil {
		t.Fatal(err)
	}
	return len(options) == 1 && options[0] == "autovacuum_vacuum_scale_factor=0.03"
}

func statesOf(t *testing.T, fx *recFixture, id int64) []recommendation.Transition {
	t.Helper()
	history, err := fx.recs.Transitions(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

// waitActionSettled waits for the rollback monitor to leave 'monitoring'.
func waitActionSettled(t *testing.T, fx *recFixture, actionID int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var outcome string
		if err := fx.pool.QueryRow(fx.ctx, `SELECT outcome FROM sage.action_log
			WHERE id=$1`, actionID).Scan(&outcome); err != nil {
			t.Fatal(err)
		}
		if outcome != "monitoring" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("action %d still monitoring", actionID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// C07: a recommendation proposed in an earlier cycle is acted on from the
// durable queue even though the analyzer's memory no longer holds it.
func TestRunCycleActsOnDurableRecommendationC07(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	fx.exec.cfg.Trust.RollbackWindowMinutes = 0 // judge the outcome at once
	rec := fx.propose(t)

	fx.exec.RunCycle(fx.ctx, false)

	got := fx.get(t, rec.ID)
	if !fx.probeApplied(t) || got.State != recommendation.StateVerifying ||
		got.ActionLogID == nil {
		t.Fatalf("after one cycle: applied=%v head=%+v, want verifying with an action",
			fx.probeApplied(t), got)
	}
	var executed string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT sql_executed FROM sage.action_log
		WHERE id=$1`, *got.ActionLogID).Scan(&executed); err != nil || executed != fx.sql {
		t.Fatalf("linked action ran %q (%v), want the revision's SQL", executed, err)
	}
	want := []recommendation.State{"proposed", "approved", "applying", "applied", "verifying"}
	history := statesOf(t, fx, rec.ID)
	if len(history) != len(want) {
		t.Fatalf("history = %+v", history)
	}
	for i, st := range want {
		if history[i].To != st {
			t.Fatalf("history[%d] = %s, want %s", i, history[i].To, st)
		}
	}
	if !strings.HasPrefix(history[1].Actor, "policy:") {
		t.Fatalf("autonomous approval actor = %q, want policy:*", history[1].Actor)
	}
	// G-P0-1: a reloption change is credited only when its targeted metric
	// (dead tuples after autovacuum runs) improves. Nothing vacuums this
	// probe table, so the monitor ends it unverifiable and the durable
	// recommendation ends inconclusive, never verified (the old "completes
	// at once" premise of this probe no longer holds by design).
	waitActionSettled(t, fx, *got.ActionLogID)
	fx.exec.RunCycle(fx.ctx, false)
	if got := fx.get(t, rec.ID); got.State != recommendation.StateInconclusive ||
		got.Verdict != "unverifiable" {
		t.Fatalf("after the monitor: state=%s verdict=%q, want inconclusive/unverifiable",
			got.State, got.Verdict)
	}
	if n := fx.actionRows(t); n != 1 {
		t.Fatalf("%d action rows for one recommendation, want 1", n)
	}
}

// autovacuumProbe2's rollback is captured at apply time (G-P0-1).
const autovacuumProbe2 = "ALTER TABLE public.{table} SET (autovacuum_vacuum_scale_factor = 0.03)"

func TestRunCycleSupersedesWhenFindingClosed(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	rec := fx.propose(t)
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE sage.findings SET status='resolved',
		resolved_at=now() WHERE id=$1`, fx.id); err != nil {
		t.Fatal(err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	if got := fx.get(t, rec.ID); got.State != recommendation.StateSuperseded ||
		got.Reason == "" {
		t.Fatalf("stale candidate: %+v, want superseded with reason", got)
	}
	if fx.probeApplied(t) || fx.actionRows(t) != 0 {
		t.Fatal("a stale recommendation was executed")
	}
}

// Decision (a): a failed apply is failed with backoff, never proposed.
func TestRunCycleFailedApplyMovesToFailedWithBackoff(t *testing.T) {
	fx := newRecFixture(t,
		"ALTER TABLE public.{table}_missing SET (autovacuum_vacuum_scale_factor = 0.03)",
		policy.VerdictExecute)
	rec := fx.propose(t)
	fx.exec.RunCycle(fx.ctx, false)
	got := fx.get(t, rec.ID)
	if got.State != recommendation.StateFailed || got.AttemptCount != 1 ||
		got.NextAttemptAt == nil || !strings.Contains(got.Reason, "does not exist") ||
		got.ActionLogID == nil {
		t.Fatalf("failed apply: %+v, want failed attempt 1 with backoff and reason", got)
	}
	fx.exec.RunCycle(fx.ctx, false)
	if n := fx.actionRows(t); n != 1 {
		t.Fatalf("retried during backoff: %d action rows, want 1", n)
	}
	for _, tr := range statesOf(t, fx, rec.ID) {
		if tr.To == recommendation.StateProposed && tr.From != "" {
			t.Fatalf("failure returned the recommendation to proposed: %+v", tr)
		}
	}
}

func TestRunCycleQueuesApprovalPinnedToRevision(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictQueueApproval)
	rec := fx.propose(t)
	mp := &mockProposer{}
	fx.exec.WithActionStore(mp, "approval")
	fx.exec.RunCycle(fx.ctx, false)
	if len(mp.calls) != 1 {
		t.Fatalf("proposals = %d, want 1", len(mp.calls))
	}
	meta := mp.calls[0].metadata
	if meta.RecommendationID != rec.ID || meta.RecommendationRevision != rec.Revision ||
		meta.ContentHash != rec.ContentHash {
		t.Fatalf("queued proposal pins %d/%d/%q, want %d/%d/%q", meta.RecommendationID,
			meta.RecommendationRevision, meta.ContentHash, rec.ID, rec.Revision, rec.ContentHash)
	}
	if got := fx.get(t, rec.ID); got.State != recommendation.StateProposed {
		t.Fatalf("queued recommendation state = %s, want proposed", got.State)
	}
}

// An operator approval recorded durably is executed by the cycle under
// the operator's authorization, and the audit keeps the approver.
func TestRunCycleExecutesOperatorApproval(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	rec := fx.propose(t)
	if _, err := fx.recs.Approve(fx.ctx, rec.ID, rec.ContentHash, "user:5"); err != nil {
		t.Fatal(err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	got := fx.get(t, rec.ID)
	if got.State != recommendation.StateVerifying || got.ApprovedBy != "user:5" ||
		got.ActionLogID == nil {
		t.Fatalf("operator approval: %+v", got)
	}
	var approvedBy *int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT approved_by FROM sage.action_log
		WHERE id=$1`, *got.ActionLogID).Scan(&approvedBy); err != nil ||
		approvedBy == nil || *approvedBy != 5 {
		t.Fatalf("action approved_by = %v (%v), want 5", approvedBy, err)
	}
}

func TestExecuteManualClaimsTheApprovedRevision(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	rec := fx.propose(t)
	if _, err := fx.recs.Approve(fx.ctx, rec.ID, rec.ContentHash, "user:8"); err != nil {
		t.Fatal(err)
	}
	eight := 8
	actionID, err := fx.exec.ExecuteManual(fx.ctx, int(fx.id), fx.sql, "", &eight)
	if err != nil || actionID <= 0 {
		t.Fatalf("ExecuteManual = %d, %v", actionID, err)
	}
	got := fx.get(t, rec.ID)
	if got.State != recommendation.StateVerifying || got.ActionLogID == nil ||
		*got.ActionLogID != actionID {
		t.Fatalf("head after manual apply = %+v, want verifying linked to %d", got, actionID)
	}
}

// Two operators race to execute one approval: exactly one runs it.
func TestExecuteManualRaceExactlyOneRuns(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	rec := fx.propose(t)
	if _, err := fx.recs.Approve(fx.ctx, rec.ID, rec.ContentHash, "user:8"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	ids := make([]int64, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := 8
			ids[i], errs[i] = fx.exec.ExecuteManual(fx.ctx, int(fx.id), fx.sql, "", &user)
		}(i)
	}
	wg.Wait()
	ran, refused := 0, 0
	for i := range errs {
		switch {
		case errs[i] == nil && ids[i] > 0:
			ran++
		// The loser is stopped by the typed-target lease on the table, or,
		// when it arrives after the winner released it, by the claim.
		case errors.Is(errs[i], recommendation.ErrConflict) ||
			errors.Is(errs[i], ErrTargetLeased):
			refused++
		}
	}
	if ran != 1 || refused != 1 || fx.actionRows(t) != 1 {
		t.Fatalf("ran=%d refused=%d rows=%d errs=%v; want exactly one run",
			ran, refused, fx.actionRows(t), errs)
	}
}

func TestRunCycleRecoversInterruptedApply(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	rec := fx.propose(t)
	if _, err := fx.recs.Claim(fx.ctx, recommendation.ClaimRequest{ID: rec.ID,
		Revision: rec.Revision, ApproveAs: "policy:test", Lease: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE sage.recommendation
		SET lease_until = now() - interval '1 second' WHERE id=$1`, rec.ID); err != nil {
		t.Fatal(err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	got := fx.get(t, rec.ID)
	if got.State != recommendation.StateFailed || !strings.Contains(got.Reason, "interrupted") {
		t.Fatalf("interrupted apply after restart: %+v, want failed/interrupted", got)
	}
	if fx.actionRows(t) != 0 {
		t.Fatal("the interrupted attempt was re-run inside its backoff")
	}
}

// proposeDurable records findings as durable recommendations for the
// default (unnamed) database, as the analyzer's cycle does. RunCycle acts
// on these rows (C07), never on the analyzer's in-memory findings. Like
// the analyzer it skips findings without SQL; a finding with no open
// sage.findings row proposes nothing, as in production. The tests using
// it run sequentially and share the unnamed database, so each starts from
// an empty queue, as each started from its own in-memory list before.
func proposeDurable(t *testing.T, pool *pgxpool.Pool, findings []analyzer.Finding) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM sage.recommendation WHERE database_name = ''`); err != nil {
		t.Fatalf("reset default recommendation queue: %v", err)
	}
	recs := recommendation.NewStore(pool)
	for _, f := range findings {
		if strings.TrimSpace(f.RecommendedSQL) == "" {
			continue
		}
		if _, err := recs.Propose(context.Background(),
			analyzer.RecommendationProposal("", f)); err != nil {
			t.Fatalf("propose %s/%s: %v", f.Category, f.ObjectIdentifier, err)
		}
	}
}
