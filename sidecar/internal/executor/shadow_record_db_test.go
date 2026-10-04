package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/store"
)

// Integration (real Postgres, real ledger): below a class's earned level
// the executor records a shadow decision for every action it would have
// taken: the exact SQL, rollback, prediction, evidence and the gate's
// verdict had the class been trusted. It never executes anything and
// never locks user objects. Once per fingerprint per window; a trusted
// (L3) class records none, a demoted one resumes. The approval queue
// works as before, with the shadow recorded alongside.

type shadowRig struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	exec     *Executor
	svc      *earned.Service
	database string
}

func newShadowRig(t *testing.T, grandfather bool) *shadowRig {
	t.Helper()
	pool, ctx := requireDB(t)
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	database := "shadow-" + time.Now().UTC().Format("150405.000000000")
	st, err := earned.NewPostgresStore(pool, deployment, database)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := earned.NewService(st, earned.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	exec := New(pool, windowedAutonomousConfig(), time.Now().Add(-60*24*time.Hour),
		noopExecLog)
	exec.WithActionStore(store.NewActionStore(pool), "auto")
	exec.WithDatabaseName(database)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	if grandfather {
		if _, err := svc.SeedGrandfathered(ctx, database, exec.OperatorBound()); err != nil {
			t.Fatal(err)
		}
	}
	exec.WithAutonomy(svc.Limiter(earned.Binding{Database: database, HA: primaryHA{},
		Concurrency: noConcurrentActions{}}))
	exec.EnableStandingPolicyDocument(unlimitedWindowPolicy(), nil)
	return &shadowRig{t: t, ctx: ctx, pool: pool, exec: exec, svc: svc, database: database}
}

// table creates a fresh table and an open stale-statistics finding for it.
func (r *shadowRig) analyzeFinding(table string) analyzer.Finding {
	r.t.Helper()
	qualified := "public." + table
	for _, sql := range []string{
		"DROP TABLE IF EXISTS " + qualified,
		"CREATE TABLE " + qualified + " (id int)",
		"INSERT INTO " + qualified + " SELECT generate_series(1, 100)",
	} {
		if _, err := r.pool.Exec(r.ctx, sql); err != nil {
			r.t.Fatalf("%s: %v", sql, err)
		}
	}
	f := analyzer.Finding{Category: "stale_statistics", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: qualified, Title: "stale statistics " + table,
		Recommendation: "analyze it", RecommendedSQL: "ANALYZE " + qualified,
		Detail: map[string]any{"n_mod_since_analyze": 100}}
	r.openFinding(f)
	r.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = r.pool.Exec(ctx, "DELETE FROM sage.shadow_decision WHERE object_identifier = $1",
			qualified)
		_, _ = r.pool.Exec(ctx, "DELETE FROM sage.action_queue WHERE proposed_sql = $1",
			f.RecommendedSQL)
		_, _ = r.pool.Exec(ctx, "DELETE FROM sage.findings WHERE object_identifier = $1",
			qualified)
		_, _ = r.pool.Exec(ctx, "DROP TABLE IF EXISTS "+qualified)
	})
	return f
}

func (r *shadowRig) openFinding(f analyzer.Finding) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql,
		status) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open')`, f.Category, f.Severity,
		f.ObjectType, f.ObjectIdentifier, f.Title, f.Detail, f.Recommendation,
		f.RecommendedSQL); err != nil {
		r.t.Fatalf("finding: %v", err)
	}
}

// shadows are the shadow decisions recorded for an object.
func (r *shadowRig) shadows(object string) []shadow.Decision {
	r.t.Helper()
	all, err := shadow.NewStore(r.pool).List(r.ctx, shadow.Filter{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	var out []shadow.Decision
	for _, d := range all {
		if d.Object == object {
			out = append(out, d)
		}
	}
	return out
}

func (r *shadowRig) actions(sql string) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, sql).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *shadowRig) setLevel(c earned.ActionClass, to earned.Level) {
	r.t.Helper()
	if _, err := r.svc.Downgrade(r.ctx, earned.DowngradeRequest{
		Family: earned.SelfFamilyFor(c), Class: c, To: to,
		Actor: "user:1:ops@example.com", Reason: "shadow test"}); err != nil {
		r.t.Fatal(err)
	}
}

func TestShadowRecordedBelowTheEarnedLevel(t *testing.T) {
	r := newShadowRig(t, false) // a fresh ledger: analyze at L1
	f := r.analyzeFinding("shadow_l1")
	r.exec.processFinding(r.ctx, f, false, nil)
	got := r.shadows(f.ObjectIdentifier)
	if len(got) != 1 {
		t.Fatalf("shadow decisions = %d, want 1", len(got))
	}
	d := got[0]
	if d.Class != "analyze" || d.Family != "hygiene" || d.SQL != f.RecommendedSQL ||
		d.Database != r.database || d.FindingID <= 0 || d.Title != f.Title {
		t.Fatalf("identity: %+v", d)
	}
	if d.GateVerdict != "observe_only" || d.GateReason != "autonomy_level" ||
		d.TrustedVerdict != "execute" || d.TrustedReason != "autonomy_l3" ||
		d.GrantedLevel != 1 || d.DecisionID <= 0 {
		t.Fatalf("gate: %+v", d)
	}
	if d.Prediction.Metric != "n_mod_since_analyze" || d.Prediction.Method != "rule" ||
		d.Prediction.ExpectedChangePct == nil {
		t.Fatalf("prediction: %+v", d.Prediction)
	}
	if d.Evidence["finding_category"] != "stale_statistics" || d.Status != shadow.StatusPending {
		t.Fatalf("evidence/status: %v %s", d.Evidence, d.Status)
	}
	if n := r.actions(f.RecommendedSQL); n != 0 {
		t.Fatalf("shadow mode executed the action (%d action_log rows)", n)
	}
	r.exec.processFinding(r.ctx, f, false, nil)
	got = r.shadows(f.ObjectIdentifier)
	if len(got) != 1 || got[0].SeenCount != 2 {
		t.Fatalf("second cycle: %d decisions, seen %d", len(got), got[0].SeenCount)
	}
}

func TestNoShadowAtL3AndShadowResumesAfterDemotion(t *testing.T) {
	r := newShadowRig(t, true) // grandfathered: analyze at L3
	trusted := r.analyzeFinding("shadow_l3")
	r.exec.processFinding(r.ctx, trusted, false, nil)
	if got := r.shadows(trusted.ObjectIdentifier); len(got) != 0 {
		t.Fatalf("a trusted class recorded %d shadow decisions", len(got))
	}
	if r.actions(trusted.RecommendedSQL) != 1 {
		t.Fatal("the trusted class did not execute")
	}
	r.setLevel(earned.ClassAnalyze, earned.L1)
	demoted := r.analyzeFinding("shadow_demoted")
	r.exec.processFinding(r.ctx, demoted, false, nil)
	got := r.shadows(demoted.ObjectIdentifier)
	if len(got) != 1 || got[0].GrantedLevel != 1 {
		t.Fatalf("after demotion: %+v", got)
	}
	if r.actions(demoted.RecommendedSQL) != 0 {
		t.Fatal("a demoted class executed")
	}
}

func TestShadowRecordedAlongsideTheApprovalQueue(t *testing.T) {
	r := newShadowRig(t, true)
	r.setLevel(earned.ClassAnalyze, earned.L2)
	f := r.analyzeFinding("shadow_l2")
	r.exec.processFinding(r.ctx, f, false, nil)
	var queued int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM sage.action_queue
		WHERE proposed_sql = $1 AND status = 'pending'`, f.RecommendedSQL).
		Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("approval queue = %d (%v), want 1 as before", queued, err)
	}
	got := r.shadows(f.ObjectIdentifier)
	if len(got) != 1 || got[0].GateVerdict != "queue_approval" ||
		got[0].GateReason != "autonomy_handoff" || got[0].TrustedVerdict != "execute" ||
		got[0].GrantedLevel != 2 {
		t.Fatalf("shadow beside the queue: %+v", got)
	}
}

func TestConcurrentCyclesRecordOneShadow(t *testing.T) {
	r := newShadowRig(t, false)
	f := r.analyzeFinding("shadow_race")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.exec.processFinding(r.ctx, f, false, nil)
		}()
	}
	wg.Wait()
	got := r.shadows(f.ObjectIdentifier)
	if len(got) != 1 || got[0].SeenCount != 4 {
		t.Fatalf("racing cycles: %d decisions (%+v)", len(got), got)
	}
}

// Shadow recording reads only catalogs, statistics views and sage tables:
// a session holding ACCESS EXCLUSIVE on the target cannot block it.
func TestShadowRecordingTakesNoLockOnUserObjects(t *testing.T) {
	r := newShadowRig(t, false)
	f := r.analyzeFinding("shadow_locked")
	conn, err := r.pool.Acquire(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(r.ctx, "LOCK TABLE "+f.ObjectIdentifier+
		" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	start := time.Now()
	r.exec.processFinding(ctx, f, false, nil)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("recording waited %s behind a lock on the user table", elapsed)
	}
	if got := r.shadows(f.ObjectIdentifier); len(got) != 1 {
		t.Fatalf("shadow decisions under a held lock = %d, want 1", len(got))
	}
}

func TestShadowCarriesTheRollbackOfAConfigChange(t *testing.T) {
	r := newShadowRig(t, false)
	object := fmt.Sprintf("work_mem_%d", time.Now().UnixNano())
	f := analyzer.Finding{Category: "config_recommendation", Severity: "info",
		ObjectType: "setting", ObjectIdentifier: object, Title: "raise work_mem",
		Recommendation: "raise it", RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'",
		Detail: map[string]any{}}
	r.openFinding(f)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = r.pool.Exec(ctx, "DELETE FROM sage.shadow_decision WHERE object_identifier = $1",
			object)
		_, _ = r.pool.Exec(ctx, "DELETE FROM sage.findings WHERE object_identifier = $1", object)
	})
	r.exec.processFinding(r.ctx, f, false, nil)
	got := r.shadows(object)
	if len(got) != 1 {
		t.Fatalf("shadow decisions = %d, want 1", len(got))
	}
	d := got[0]
	if d.Class != "config_guc" || d.RollbackSQL == "" || d.Prediction.Metric != "temp_spills" {
		t.Fatalf("config shadow: class=%s rollback=%q prediction=%+v", d.Class,
			d.RollbackSQL, d.Prediction)
	}
	if r.actions(f.RecommendedSQL) != 0 {
		t.Fatal("shadow mode ran ALTER SYSTEM")
	}
}

// Without a ledger that can name the granted level (enforce off, a fake
// limiter, a failing ledger) nothing is shadowed; hard stops are not
// shadowed either (the class's trust is not what withholds the action).
func TestNoShadowWithoutALedgerOrOnAHardStop(t *testing.T) {
	// One rig: requireDB holds the package's cross-package lock per test.
	r := newShadowRig(t, false)
	stopped := r.analyzeFinding("shadow_estop")
	r.exec.WithEmergencyStopCheck(func(context.Context) bool { return true })
	r.exec.processFinding(r.ctx, stopped, false, nil)
	if got := r.shadows(stopped.ObjectIdentifier); len(got) != 0 {
		t.Fatalf("an emergency stop recorded %d shadows", len(got))
	}
	r.exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	f := r.analyzeFinding("shadow_none")
	r.exec.WithAutonomy(&scopedCountingLimiter{limit: policy.AutonomyLimit{Level: 1,
		Granted: 1}})
	r.exec.EnableStandingPolicyDocument(unlimitedWindowPolicy(), nil)
	r.exec.processFinding(r.ctx, f, false, nil)
	if got := r.shadows(f.ObjectIdentifier); len(got) != 0 {
		t.Fatalf("a limiter without a granted level recorded %d shadows", len(got))
	}
}

// Custodian work without an incident family is self-initiated (roadmap
// 1.2) and shadowed like a finding when withheld; an incident-family
// remediation (freeze: wraparound runway) earns from its own evidence and
// is not.
func TestShadowRecordsWithheldCustodianWork(t *testing.T) {
	r := newShadowRig(t, false)
	f := r.analyzeFinding("shadow_custodian")
	err := r.exec.SubmitCustodianProposal(r.ctx, CustodianProposal{Feature: "analyze",
		SQL: "ANALYZE " + f.ObjectIdentifier, TargetObjects: []string{f.ObjectIdentifier},
		ObservedAt: time.Now()})
	if err == nil {
		t.Fatal("an L1 custodian action was not withheld")
	}
	got := r.shadows(f.ObjectIdentifier)
	if len(got) != 1 || got[0].Class != "analyze" || got[0].FindingID != 0 ||
		got[0].TrustedVerdict != "execute" || got[0].Title != "analyze custodian action" {
		t.Fatalf("custodian shadow: %+v", got)
	}
	if r.actions("ANALYZE "+f.ObjectIdentifier) != 0 {
		t.Fatal("a withheld custodian action ran")
	}
	freeze := r.analyzeFinding("shadow_freeze")
	_ = r.exec.SubmitCustodianProposal(r.ctx, CustodianProposal{Feature: "freeze",
		SQL: `VACUUM (FREEZE) ` + freeze.ObjectIdentifier,
		TargetObjects: []string{freeze.ObjectIdentifier}, ObservedAt: time.Now()})
	if got := r.shadows(freeze.ObjectIdentifier); len(got) != 0 {
		t.Fatalf("an incident-family remediation was shadowed: %+v", got)
	}
}

// A class trusted at L3 is not shadowed even when the operator's ceiling
// (execution_mode approval) still queues its action for a person.
func TestNoShadowForATrustedClassHeldByTheCeiling(t *testing.T) {
	r := newShadowRig(t, true)
	r.exec.SetExecutionMode("approval")
	f := r.analyzeFinding("shadow_ceiling")
	r.exec.processFinding(r.ctx, f, false, nil)
	if got := r.shadows(f.ObjectIdentifier); len(got) != 0 {
		t.Fatalf("an L3 class held by the ceiling was shadowed: %+v", got)
	}
}

// Once a decision is scored, the same finding is not recorded again
// inside the dedupe window: one decision per fingerprint per window.
func TestNoSecondShadowInsideTheWindowAfterScoring(t *testing.T) {
	r := newShadowRig(t, false)
	f := r.analyzeFinding("shadow_window")
	r.exec.processFinding(r.ctx, f, false, nil)
	if _, err := r.pool.Exec(r.ctx, `UPDATE sage.shadow_decision SET status = 'scored',
		score = 'unscored', score_source = 'none', scored_at = now()
		WHERE object_identifier = $1`, f.ObjectIdentifier); err != nil {
		t.Fatal(err)
	}
	r.exec.processFinding(r.ctx, f, false, nil)
	if got := r.shadows(f.ObjectIdentifier); len(got) != 1 {
		t.Fatalf("decisions inside the window after scoring = %d, want 1", len(got))
	}
}

// trust.level observation is the ceiling: the gate stops before the
// ledger, yet pg_sage still records what it would have done (that is how
// a new install earns trust), with the ceiling's verdict as the verdict
// had the class been trusted.
func TestShadowRecordedUnderTheObservationCeiling(t *testing.T) {
	r := newShadowRig(t, false)
	if err := r.exec.SetTrustLevel("observation"); err != nil {
		t.Fatal(err)
	}
	f := r.analyzeFinding("shadow_observation")
	r.exec.processFinding(r.ctx, f, false, nil)
	got := r.shadows(f.ObjectIdentifier)
	if len(got) != 1 || got[0].GateVerdict != "observe_only" ||
		got[0].GateReason != "observe_only" || got[0].TrustedVerdict != "observe_only" ||
		got[0].GrantedLevel != 1 {
		t.Fatalf("shadow under the observation ceiling: %+v", got)
	}
}
