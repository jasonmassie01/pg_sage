package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-01 on real PostgreSQL with the production standing gate (the
// durable policy store and decision ledger), never allowRollback: an
// autonomous drop whose targeted query regresses is restored CONCURRENTLY
// under the restore contract, whatever trust was lowered to since.

// restoreFixture is an autonomous drop_unused_index that ran two hours ago
// through the production gate, run and log path.
type restoreFixture struct {
	e                 *Executor
	pool              *pgxpool.Pool
	ctx               context.Context
	finding           analyzer.Finding
	table, index      string
	findingID, action int64
	qid               int64
	spy               *restoreSpy
}

// restoreSpy records the SQL each rollback ran and runs it for real.
type restoreSpy struct {
	mu   sync.Mutex
	sqls []string
	real func(context.Context, string, time.Duration, ...DDLOption) error
}

func (s *restoreSpy) exec(
	ctx context.Context, sql string, timeout time.Duration, opts ...DDLOption,
) error {
	s.mu.Lock()
	s.sqls = append(s.sqls, sql)
	s.mu.Unlock()
	return s.real(ctx, sql, timeout, opts...)
}

func (s *restoreSpy) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sqls...)
}

func newRestoreExecutor(t *testing.T, pool *pgxpool.Pool, ctx context.Context) *Executor {
	t.Helper()
	e := New(pool, restoreTrustConfig(), time.Now().Add(-90*24*time.Hour), noopExecLog)
	if err := e.EnableStandingPolicy(ctx, "unattended", nil); err != nil {
		t.Fatalf("standing policy: %v", err)
	}
	e.WithActionStore(store.NewActionStore(pool), "auto")
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.Shutdown(sctx)
	})
	return e
}

// newRestoreFixture builds the table, its index and the drop finding with
// the rule's own rollback (pg_get_indexdef + ";"), authorizes and runs the
// drop at autonomous trust, then dates it two hours back with a frozen
// baseline so the monitor judges it at once.
func newRestoreFixture(t *testing.T, unique bool) restoreFixture {
	t.Helper()
	pool, ctx := isolatedSageDB(t)
	f := restoreFixture{e: newRestoreExecutor(t, pool, ctx), pool: pool, ctx: ctx,
		table: fmt.Sprintf("dr_%d", time.Now().UnixNano())}
	f.index = f.table + "_a"
	kind := "INDEX"
	if unique {
		kind = "UNIQUE INDEX"
	}
	mustExec(t, pool, ctx, "CREATE TABLE public."+f.table+" (a int)")
	mustExec(t, pool, ctx, "CREATE "+kind+" "+f.index+" ON public."+f.table+" (a)")
	def := indexDef(t, ctx, pool, f.index)
	f.finding = analyzer.Finding{Category: "unused_index", Severity: "warning",
		ObjectType: "index", ObjectIdentifier: "public." + f.index,
		Title:          "Unused index public." + f.index,
		Detail:         map[string]any{"ladder": rehearsedLadder()},
		RecommendedSQL: `DROP INDEX CONCURRENTLY "public"."` + f.index + `";`,
		RollbackSQL:    def + ";", ActionRisk: "safe"}
	f.findingID = insertDropFinding(t, f)
	if err := prepareFindingRollback(&f.finding); err != nil {
		t.Fatalf("prepare rollback: %v", err)
	}
	f.action = f.runDrop(t)
	f.qid = int64(8_760_000_000) + time.Now().UnixNano()%1_000_000
	f.backdate(t, time.Now().Add(-2*time.Hour).UTC())
	f.spy = &restoreSpy{real: RollbackMonitorConfig{}.exec(pool)}
	return f
}

func insertDropFinding(t *testing.T, f restoreFixture) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql, rollback_sql)
		VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6, $7) RETURNING id`,
		f.finding.Category, f.finding.Severity, f.finding.ObjectType,
		f.finding.ObjectIdentifier, f.finding.Title, f.finding.RecommendedSQL,
		f.finding.RollbackSQL).Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id
}

// runDrop authorizes the drop through the real gate, runs and logs it.
func (f restoreFixture) runDrop(t *testing.T) int64 {
	t.Helper()
	decision := f.e.evaluateFindingPolicy(f.ctx, f.finding, false)
	if decision.Decision != PolicyDecisionExecute {
		t.Fatalf("autonomous drop not authorized: %+v", decision)
	}
	if err := f.e.runFindingSQL(f.ctx, f.finding, decision); err != nil {
		t.Fatalf("run drop: %v", err)
	}
	id := f.e.logClaimedAction(f.ctx, f.finding, f.findingID, map[string]any{},
		decision.DecisionID, nil, nil)
	if id <= 0 {
		t.Fatal("drop was not logged")
	}
	if def := indexDef(t, f.ctx, f.pool, f.index); def != "" {
		t.Fatalf("index still exists after the drop: %s", def)
	}
	return id
}

// backdate moves the drop to at with a frozen baseline and a regression of
// its targeted query since (3x the baseline mean).
func (f restoreFixture) backdate(t *testing.T, at time.Time) {
	t.Helper()
	p := rulePrediction(0)
	p.Class, p.Metric, p.TargetQueryIDs = verify.ClassIndexDrop,
		verify.MetricMeanExecTime, []int64{f.qid}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log SET executed_at = $2,
		before_state = $3::jsonb, outcome = 'monitoring' WHERE id = $1`, f.action, at,
		beforeStateJSON(p, fixtureBaselineMs, nil)); err != nil {
		t.Fatalf("backdate drop: %v", err)
	}
	seedQuerySamples(t, f.ctx, f.pool, f.qid, at.Add(time.Second), 110, 40,
		fixtureBaselineMs*3)
}

// monitor runs the production monitor config of the drop, with the spy
// over the real rollback execution.
func (f restoreFixture) monitor() {
	cfg := f.e.findingMonitorConfig(f.finding, f.action)
	cfg.execRollback = f.spy.exec
	MonitorAndRollback(f.ctx, f.pool, f.action, f.finding.RollbackSQL, cfg, noopExecLog, nil)
}

func (f restoreFixture) outcome(t *testing.T) (string, string) {
	t.Helper()
	outcome, _ := actionOutcomeFor(t, f.pool, f.action)
	return outcome, actionReason(t, f.ctx, f.pool, f.action)
}

// assertRestored checks CHECK-01: rolled_back, and the index is back and
// valid, built by one CONCURRENTLY statement.
func (f restoreFixture) assertRestored(t *testing.T) {
	t.Helper()
	if outcome, reason := f.outcome(t); outcome != "rolled_back" {
		t.Fatalf("outcome = %q (%s), want rolled_back", outcome, reason)
	}
	var valid bool
	if err := f.pool.QueryRow(f.ctx, `SELECT i.indisvalid FROM pg_index i
		WHERE i.indexrelid = to_regclass($1)`, "public."+f.index).Scan(&valid); err != nil ||
		!valid {
		t.Fatalf("restored index valid = %v (%v), want a valid index", valid, err)
	}
	calls := f.spy.calls()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "CREATE INDEX CONCURRENTLY ") &&
		!strings.HasPrefix(calls[0], "CREATE UNIQUE INDEX CONCURRENTLY ") {
		t.Fatalf("rollback statements = %q, want one CREATE INDEX CONCURRENTLY", calls)
	}
}

// restoreDecision is the latest restore decision in the ledger.
func (f restoreFixture) restoreDecision(t *testing.T) (verdict, reason, gate string) {
	t.Helper()
	if err := f.pool.QueryRow(f.ctx, `SELECT verdict, reason,
		COALESCE(evidence->>'gate_reason', '') FROM sage.decision
		WHERE evidence->>'proposed_sql' = $1 ORDER BY id DESC LIMIT 1`, f.finding.RollbackSQL).
		Scan(&verdict, &reason, &gate); err != nil {
		t.Fatalf("restore decision: %v", err)
	}
	return verdict, reason, gate
}

// CHECK-01/CHECK-02: at autonomous trust the regressed drop is restored
// CONCURRENTLY, the stored rollback is the CONCURRENTLY form, and the
// ledger records the restore bypass.
func TestDropRestoreAtAutonomousTrust(t *testing.T) {
	f := newRestoreFixture(t, false)
	var stored string
	if err := f.pool.QueryRow(f.ctx, `SELECT rollback_sql FROM sage.action_log
		WHERE id = $1`, f.action).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "CREATE INDEX CONCURRENTLY ") ||
		strings.HasSuffix(stored, ";") {
		t.Fatalf("stored rollback = %q, want the CONCURRENTLY restore", stored)
	}

	f.monitor()

	f.assertRestored(t)
	verdict, reason, gate := f.restoreDecision(t)
	if verdict != "execute" || gate != "drop_restore_bypass" ||
		!strings.Contains(reason, "restore of an index pg_sage dropped") {
		t.Fatalf("restore decision = %s/%s (%s), want execute by the restore bypass",
			verdict, gate, reason)
	}
	got := storedOutcome(t, f.pool, f.action)
	if rb, _ := got.Evidence["rollback"].(map[string]any); rb["restored"] != true {
		t.Fatalf("outcome evidence = %+v, want the restore recorded", got.Evidence)
	}
}

// State transitions: after the drop, trust is lowered in each documented
// way; the regression still restores the index, and the ledger names the
// bypassed downgrade.
func TestDropRestoreAfterTrustDowngrades(t *testing.T) {
	cases := []struct {
		name, detail string
		downgrade    func(*Executor)
	}{
		{"observation", "trust observation", func(e *Executor) {
			_ = e.SetTrustLevel("observation")
		}},
		{"approval mode", "execution_mode approval", func(e *Executor) {
			e.SetExecutionMode("approval")
		}},
		{"tier3_safe off", "tier3_safe off", func(e *Executor) { e.cfg.Trust.Tier3Safe = false }},
		{"ramp reset", "trust ramp not satisfied", func(e *Executor) { e.rampStart = time.Now() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRestoreFixture(t, false)
			c.downgrade(f.e)

			f.monitor()

			f.assertRestored(t)
			verdict, reason, gate := f.restoreDecision(t)
			if verdict != "execute" || gate != "drop_restore_bypass" ||
				!strings.Contains(reason, c.detail) {
				t.Fatalf("restore decision = %s/%s (%s), want the bypass of %q", verdict,
					gate, reason, c.detail)
			}
		})
	}
}

// State transition: the emergency stop withholds the restore visibly (one
// approval card, no DDL); after the stop is lifted, approving the card
// restores the index.
func TestDropRestoreEmergencyStopQueuesOneCard(t *testing.T) {
	f := newRestoreFixture(t, false)
	if err := SetEmergencyStop(f.ctx, f.pool, true, "test"); err != nil {
		t.Fatal(err)
	}

	f.monitor()
	f.monitor() // a second monitor for the same action adds nothing

	if outcome, reason := f.outcome(t); outcome != "rollback_withheld" ||
		!strings.HasPrefix(reason, "emergency_stop") {
		t.Fatalf("outcome = %q (%s), want rollback_withheld by emergency_stop", outcome, reason)
	}
	if calls := f.spy.calls(); len(calls) != 0 {
		t.Fatalf("DDL ran during the emergency stop: %q", calls)
	}
	item := f.restoreCard(t)
	if r := f.e.ApprovalReadinessWithEvidence(item, time.Now(), false); r.Eligible {
		t.Fatalf("card approvable during the stop: %+v", r)
	}

	if err := SetEmergencyStop(f.ctx, f.pool, false, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.supersedeStaleApprovals(f.ctx); err != nil {
		t.Fatal(err)
	}
	item = f.restoreCard(t)
	if r := f.e.ApprovalReadinessWithEvidence(item, time.Now(), false); !r.Eligible {
		t.Fatalf("card not approvable after the stop: %s", r.DeferReason)
	}
	run, err := f.e.RunApprovedAction(f.ctx, item, 1)
	if err != nil || run.ActionLogID != f.action {
		t.Fatalf("approved restore = %+v, %v", run, err)
	}
	if outcome, reason := f.outcome(t); outcome != "rolled_back" {
		t.Fatalf("outcome after approval = %q (%s), want rolled_back", outcome, reason)
	}
	if def := indexDef(t, f.ctx, f.pool, f.index); !strings.Contains(def, "(a)") {
		t.Fatalf("approved restore did not re-create the index: %q", def)
	}
	if _, err := f.e.RunApprovedAction(f.ctx, item, 1); err == nil {
		t.Fatal("a settled restore card ran a second time")
	}
}

// restoreCard is the single pending restore card of the fixture's drop.
func (f restoreFixture) restoreCard(t *testing.T) store.QueuedAction {
	t.Helper()
	var ids []int
	rows, err := f.pool.Query(f.ctx, `SELECT id FROM sage.action_queue
		WHERE identity_key = $1 AND status = 'pending'`, restoreIdentityKey(f.action))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 {
		t.Fatalf("pending restore cards = %v, want exactly one", ids)
	}
	item, err := store.NewActionStore(f.pool).GetByID(f.ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if item.ActionType != ActionTypeRestoreDroppedIndex ||
		item.ProposedSQL != f.finding.RollbackSQL || item.FindingID != int(f.findingID) {
		t.Fatalf("restore card = %+v", item)
	}
	return *item
}

// A card whose action was rolled back some other way is superseded, and
// it never runs.
func TestDropRestoreCardSupersededOnceSettled(t *testing.T) {
	f := newRestoreFixture(t, false)
	if err := SetEmergencyStop(f.ctx, f.pool, true, "test"); err != nil {
		t.Fatal(err)
	}
	f.monitor()
	item := f.restoreCard(t)
	if err := SetEmergencyStop(f.ctx, f.pool, false, "test"); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RollbackAction(f.ctx, f.action, "operator"); err != nil {
		t.Fatalf("operator rollback: %v", err)
	}
	if n, err := f.e.supersedeStaleApprovals(f.ctx); err != nil || n < 1 {
		t.Fatalf("superseded = %d, %v, want the settled card closed", n, err)
	}
	if _, err := f.e.RunApprovedAction(f.ctx, item, 1); err == nil {
		t.Fatal("the restore card of a rolled-back action ran")
	}
}

// Invalid input: a restore that differs from the recorded definition, or
// targets another table, is refused as rollback_sql_mismatch, before the
// gate and without DDL.
func TestDropRestoreRefusesMismatchedSQL(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"other definition": func(s string) string { return strings.Replace(s, "(a)", "(a, a)", 1) },
		"other table": func(s string) string {
			return strings.Replace(s, "ON public.", "ON public.x", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRestoreFixture(t, false)
			cfg := f.e.findingMonitorConfig(f.finding, f.action)
			cfg.execRollback = f.spy.exec

			MonitorAndRollback(f.ctx, f.pool, f.action, mutate(f.finding.RollbackSQL), cfg,
				noopExecLog, nil)

			outcome, reason := f.outcome(t)
			if outcome != "rollback_skipped" || !strings.HasPrefix(reason, rollbackSQLMismatch) {
				t.Fatalf("outcome = %q (%s), want refused as %s", outcome, reason,
					rollbackSQLMismatch)
			}
			if calls := f.spy.calls(); len(calls) != 0 {
				t.Fatalf("mismatched restore ran: %q", calls)
			}
			var n int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.decision
				WHERE evidence->>'proposed_sql' LIKE 'CREATE%'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("mismatched restore reached the gate: %d decisions (%v)", n, err)
			}
		})
	}
}

// Concurrency: two monitors for the same action run one restore (the
// rolling_back compare-and-set claims the action).
func TestDropRestoreTwoMonitorsRestoreOnce(t *testing.T) {
	f := newRestoreFixture(t, false)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.monitor()
		}()
	}
	wg.Wait()

	f.assertRestored(t)
}

// Error propagation: a CONCURRENTLY build that fails (duplicates arrived
// while the unique index was gone) is rollback_failed with the error.
func TestDropRestoreFailedBuildIsRollbackFailed(t *testing.T) {
	f := newRestoreFixture(t, true)
	mustExec(t, f.pool, f.ctx, "INSERT INTO public."+f.table+" VALUES (1), (1)")

	f.monitor()

	outcome, reason := f.outcome(t)
	if outcome != "rollback_failed" || !strings.Contains(reason, "could not create unique") {
		t.Fatalf("outcome = %q (%s), want rollback_failed with the build error", outcome,
			reason)
	}
	if calls := f.spy.calls(); len(calls) != 1 ||
		!strings.HasPrefix(calls[0], "CREATE UNIQUE INDEX CONCURRENTLY ") {
		t.Fatalf("rollback statements = %q, want the one failed CONCURRENTLY build", calls)
	}
}

// Boundary: an INVALID index of the same name (a failed build left it) is
// never read as "already restored"; the restore fails visibly instead.
func TestDropRestoreInvalidLeftoverIsNotRestored(t *testing.T) {
	f := newRestoreFixture(t, true)
	mustExec(t, f.pool, f.ctx, "INSERT INTO public."+f.table+" VALUES (1), (1)")
	if _, err := f.pool.Exec(f.ctx, f.finding.RollbackSQL); err == nil {
		t.Fatal("the unique build over duplicates succeeded; no invalid leftover")
	}

	f.monitor()

	if outcome, reason := f.outcome(t); outcome != "rollback_failed" {
		t.Fatalf("outcome = %q (%s), want rollback_failed over an INVALID index", outcome,
			reason)
	}
	var valid bool
	if err := f.pool.QueryRow(f.ctx, `SELECT indisvalid FROM pg_index
		WHERE indexrelid = to_regclass($1)`, "public."+f.index).Scan(&valid); err != nil ||
		valid {
		t.Fatalf("leftover index valid = %v (%v), want it left INVALID", valid, err)
	}
}

// Rollout: a drop logged before executor-01 holds the raw definition; on
// resume after a restart it is normalized and restored by the real gate.
func TestDropRestoreResumesLegacyRawRollback(t *testing.T) {
	f := newRestoreFixture(t, false)
	raw := strings.Replace(f.finding.RollbackSQL, " CONCURRENTLY", "", 1) + ";"
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log SET rollback_sql = $2,
		outcome = 'interrupted' WHERE id = $1`, f.action, raw); err != nil {
		t.Fatal(err)
	}
	restarted := newRestoreExecutor(t, f.pool, f.ctx)
	_ = restarted.SetTrustLevel("observation")

	if err := restarted.resumeOrphanedMonitors(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	restarted.monitors.Wait()

	if outcome, reason := f.outcome(t); outcome != "rolled_back" {
		t.Fatalf("resumed outcome = %q (%s), want rolled_back", outcome, reason)
	}
	if def := indexDef(t, f.ctx, f.pool, f.index); !strings.Contains(def, "(a)") {
		t.Fatalf("resumed restore did not re-create the index: %q", def)
	}
}
