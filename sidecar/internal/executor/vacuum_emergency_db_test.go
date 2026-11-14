package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-05b: the emergency VACUUM profile runs with vacuum_cost_delay 0
// on its dedicated session, records the value it replaced, resets it, and
// escalates its own failure streak once. The autonomy package counts
// failures by the stored vacuum_cap prefix, pinned here. No concurrent
// access tests beyond the existing ones: the escalation advisory lock is
// unchanged (TestCustodianBackoffConcurrentSubmittersEscalateOnce).

func emergencySQLFor(f *vacuumFixture) string {
	return freeze.EmergencyVacuumSQL("public", f.table[len("public."):], 0, 0,
		freeze.AggressiveAges{})
}

func custodianFixtureGate(t *testing.T, f *vacuumFixture) {
	t.Helper()
	decisionID := recordCustodianDecision(t, f.ctx, f.pool, "freeze", f.table[len("public."):])
	f.exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe, DecisionID: decisionID}})
}

type storedAction struct {
	outcome, reason, costDelay string
	hasCostDelay               bool
}

func lastStoredAction(t *testing.T, f *vacuumFixture, sql string) storedAction {
	t.Helper()
	var a storedAction
	err := f.pool.QueryRow(f.ctx, `SELECT outcome, COALESCE(rollback_reason, ''),
		COALESCE(before_state->>'vacuum_cost_delay_before', ''),
		COALESCE(before_state ? 'vacuum_cost_delay_before', false)
		FROM sage.action_log WHERE sql_executed = $1 ORDER BY id DESC LIMIT 1`, sql).
		Scan(&a.outcome, &a.reason, &a.costDelay, &a.hasCostDelay)
	if err != nil {
		t.Fatalf("read the action row of %q: %v", sql, err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			"DELETE FROM sage.action_log WHERE sql_executed = $1", sql)
	})
	return a
}

// Format pin: the stored reason of a capped VACUUM starts with the outcome
// the autonomy package filters on.
func TestCapFailuresFormatPinOfVacuumWatchError(t *testing.T) {
	err := &VacuumWatchError{Outcome: VacuumCap, PID: 7, Err: errors.New("canceled")}
	if !strings.HasPrefix(err.Error(), "vacuum_cap:") {
		t.Fatalf("VacuumWatchError.Error() = %q, want the vacuum_cap: prefix", err.Error())
	}
	if VacuumCap != freeze.VacuumCapOutcome {
		t.Fatalf("VacuumCap = %q, freeze.VacuumCapOutcome = %q; they must agree", VacuumCap,
			freeze.VacuumCapOutcome)
	}
}

// Integration: a real cap through the custodian path stores a failed row
// whose rollback_reason starts with vacuum_cap:.
func TestCapFailuresPrefixStoredByACappedCustodianVacuum(t *testing.T) {
	f := newVacuumFixture(t, 40000, "50ms")
	f.exec.vacuumHooks.cap = 700 * time.Millisecond
	custodianFixtureGate(t, f)
	sql := freeze.VacuumFreezeSQL("public", f.table[len("public."):])
	_, err := f.exec.SubmitCustodianProposalDecision(f.ctx, redVacuumProposal(f.table, sql))
	var watch *VacuumWatchError
	if !errors.As(err, &watch) || watch.Outcome != VacuumCap {
		t.Fatalf("submit error = %v, want a vacuum_cap VacuumWatchError", err)
	}
	a := lastStoredAction(t, f, sql)
	if a.outcome != "failed" || !strings.HasPrefix(a.reason, freeze.VacuumCapOutcome+":") {
		t.Fatalf("stored row = %+v, want failed with a vacuum_cap: reason", a)
	}
	if a.hasCostDelay {
		t.Fatalf("normal VACUUM recorded vacuum_cost_delay_before = %q", a.costDelay)
	}
}

// Happy path: the emergency profile runs with vacuum_cost_delay 0 (the
// session's raised 20ms would hold it for many seconds), completes,
// advances relfrozenxid and records the replaced value.
func TestCustodianEmergencyVacuumDropsCostDelayAndRecordsIt(t *testing.T) {
	f := newVacuumFixture(t, 40000, "20ms")
	custodianFixtureGate(t, f)
	sql := emergencySQLFor(f)
	start := time.Now()
	if _, err := f.exec.SubmitCustodianProposalDecision(f.ctx,
		redVacuumProposal(f.table, sql)); err != nil {
		t.Fatalf("emergency VACUUM: %v", err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("emergency VACUUM took %s; the 20ms cost delay was not dropped", took)
	}
	a := lastStoredAction(t, f, sql)
	if a.outcome == "failed" || a.costDelay != "20ms" {
		t.Fatalf("stored row = %+v, want a completed run with vacuum_cost_delay_before 20ms", a)
	}
	var age int64
	if err := f.pool.QueryRow(f.ctx, `SELECT age(relfrozenxid)::bigint FROM pg_class
		WHERE oid = to_regclass($1)`, f.table).Scan(&age); err != nil {
		t.Fatalf("read age: %v", err)
	}
	if age > 1000 {
		t.Fatalf("relfrozenxid age after the emergency freeze = %d, want it advanced", age)
	}
}

// Nil/empty and boundary: a normal VACUUM leaves vacuum_cost_delay alone
// and records nothing; an emergency one records the session's value even
// when it is already 0.
func TestWatchedVacuumRecordsCostDelayOnlyForEmergency(t *testing.T) {
	f := newVacuumFixture(t, 2000, "0")
	normal := map[string]any{}
	if err := f.exec.execConcurrentDDL(f.ctx, freeze.VacuumFreezeSQL("public",
		f.table[len("public."):]), normal, WithLockTimeout(4321)); err != nil {
		t.Fatalf("normal VACUUM: %v", err)
	}
	if _, ok := normal[vacuumCostDelayBeforeKey]; ok {
		t.Fatalf("normal VACUUM state = %#v, want no cost delay key", normal)
	}
	emergency := map[string]any{}
	if err := f.exec.execConcurrentDDL(f.ctx, emergencySQLFor(f), emergency,
		WithLockTimeout(4321)); err != nil {
		t.Fatalf("emergency VACUUM: %v", err)
	}
	if got := emergency[vacuumCostDelayBeforeKey]; got != "0" {
		t.Fatalf("emergency state %s = %#v, want \"0\"", vacuumCostDelayBeforeKey, got)
	}
	if err := f.exec.execConcurrentDDL(f.ctx, emergencySQLFor(f), nil,
		WithLockTimeout(4321)); err != nil {
		t.Fatalf("emergency VACUUM with a nil state: %v", err)
	}
}

// The emergency session resets vacuum_cost_delay with its other settings,
// and a failed reset discards the connection.
func TestEmergencySessionResetsCostDelay(t *testing.T) {
	sess := &fakeSession{}
	o := applyDDLOpts([]DDLOption{WithLockTimeout(5000)})
	o.resetCostDelay = true
	if err := runTopLevelSession(context.Background(), sess, time.Minute, o,
		func(pooledExecutor) error { return nil }); err != nil {
		t.Fatalf("runTopLevelSession: %v", err)
	}
	want := append(slices.Clone(pinnedTopLevelSet), topLevelResets...)
	want = append(want, "RESET vacuum_cost_delay")
	if !slices.Equal(sess.execs, want) || sess.released != 1 {
		t.Fatalf("session statements = %q (released %d), want %q", sess.execs,
			sess.released, want)
	}
	failing := &fakeSession{failOn: "RESET vacuum_cost_delay"}
	err := runTopLevelSession(context.Background(), failing, time.Minute, o,
		func(pooledExecutor) error { return nil })
	if !errors.Is(err, errFakeSession) || failing.discarded != 1 || failing.released != 0 {
		t.Fatalf("failed reset: err %v discarded %d released %d, want a discard", err,
			failing.discarded, failing.released)
	}
	plain := &fakeSession{}
	if err := runTopLevelSession(context.Background(), plain, time.Minute,
		applyDDLOpts(nil), func(pooledExecutor) error { return nil }); err != nil {
		t.Fatalf("runTopLevelSession: %v", err)
	}
	if slices.Contains(plain.execs, "RESET vacuum_cost_delay") {
		t.Fatalf("a non-emergency session reset vacuum_cost_delay: %q", plain.execs)
	}
}

// State transition: the plain streak escalated once; a failing emergency
// streak escalates exactly once more, with profile emergency.
func TestEmergencyStreakEscalatesOnceMore(t *testing.T) {
	exec, pool, d := backoffExecutor(t)
	table, plainSQL := uniqueBackoffSQL()
	seedFailures(t, pool, plainSQL, custodianMaxFailures)
	_, _ = exec.SubmitCustodianProposalDecision(context.Background(),
		redVacuumProposal(table, plainSQL))
	if keys := escalations(d); len(keys) != 1 {
		t.Fatalf("plain streak escalations = %v, want one", keys)
	}
	emergencySQL := "VACUUM (INDEX_CLEANUP OFF, TRUNCATE OFF) " + table
	seedFailures(t, pool, emergencySQL, custodianMaxFailures)
	proposal := redVacuumProposal(table, emergencySQL)
	proposal.Evidence = map[string]any{"profile": freeze.ProfileEmergency, "cap_failures": 2}
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := exec.SubmitCustodianProposalDecision(context.Background(), proposal)
		if !errors.Is(err, ErrCustodianBackoff) {
			t.Fatalf("emergency attempt %d: error %v, want the exhausted backoff", attempt, err)
		}
	}
	if keys := escalations(d); len(keys) != 2 {
		t.Fatalf("escalations = %v, want the plain one and exactly one emergency", keys)
	}
	d.mu.Lock()
	first, last := d.events[0], d.events[len(d.events)-1]
	d.mu.Unlock()
	if _, ok := first.Data["profile"]; ok {
		t.Fatalf("plain escalation data = %#v, want no profile", first.Data)
	}
	if last.Data["profile"] != freeze.ProfileEmergency || last.Data["sql"] != emergencySQL {
		t.Fatalf("emergency escalation data = %#v", last.Data)
	}
	var profile string
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(evidence->>'profile', '')
		FROM sage.decision WHERE reason = 'custodian_backoff'
		  AND evidence->>'proposed_sql' = $1`, emergencySQL).Scan(&profile); err != nil {
		t.Fatalf("read the emergency escalation decision: %v", err)
	}
	if profile != freeze.ProfileEmergency {
		t.Fatalf("emergency escalation decision profile = %q", profile)
	}
}

// The emergency session's RESET reaches the live pooled connection: a
// role whose vacuum_cost_delay is raised to 7ms (the production case) runs
// the emergency VACUUM on a one-connection pool; the same backend records
// 7ms, and returns to the pool with 7ms again (not discarded, not left at 0).
func TestEmergencyVacuumResetsCostDelayOnTheLivePooledConnection(t *testing.T) {
	f := newVacuumFixture(t, 2000, "0")
	one := throttledRolePool(t, f, "7ms")
	exec := New(one, f.exec.cfg, time.Time{}, nopLog)
	exec.emergencyStopFn = f.exec.emergencyStopFn
	exec.vacuumHooks = vacuumHooks{interval: 50 * time.Millisecond,
		readProgress: func(context.Context, uint32) (vacuumProgress, bool, error) {
			return vacuumProgress{}, false, nil
		},
		emergencyStop: func(context.Context) (bool, error) { return false, nil }}
	pidBefore, delayBefore := sessionCostDelay(t, f.ctx, one)
	if delayBefore != "7ms" {
		t.Fatalf("role vacuum_cost_delay = %q, want 7ms", delayBefore)
	}
	state := map[string]any{}
	if err := exec.execConcurrentDDL(f.ctx, emergencySQLFor(f), state,
		WithLockTimeout(4321)); err != nil {
		t.Fatalf("emergency VACUUM: %v", err)
	}
	if got := state[vacuumCostDelayBeforeKey]; got != "7ms" {
		t.Fatalf("recorded %s = %#v, want the role's 7ms", vacuumCostDelayBeforeKey, got)
	}
	pidAfter, delayAfter := sessionCostDelay(t, f.ctx, one)
	if pidAfter != pidBefore || delayAfter != "7ms" {
		t.Fatalf("after the emergency VACUUM: backend %d vacuum_cost_delay %q, want "+
			"backend %d with 7ms", pidAfter, delayAfter, pidBefore)
	}
}

// throttledRolePool is a one-connection pool of a new superuser role whose
// own vacuum_cost_delay is delay; the role is dropped after the test.
func throttledRolePool(t *testing.T, f *vacuumFixture, delay string) *pgxpool.Pool {
	t.Helper()
	role := fmt.Sprintf("vac05b_%d", time.Now().UnixNano())
	if _, err := f.pool.Exec(f.ctx, "CREATE ROLE "+role+
		" LOGIN SUPERUSER PASSWORD 'vac05b'"); err != nil {
		t.Fatalf("create role %s: %v", role, err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role) })
	if _, err := f.pool.Exec(f.ctx, "ALTER ROLE "+role+" SET vacuum_cost_delay = '"+
		delay+"'"); err != nil {
		t.Fatalf("raise vacuum_cost_delay of %s: %v", role, err)
	}
	cfg, err := pgxpool.ParseConfig(f.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the test DSN: %v", err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password, cfg.MaxConns = role, "vac05b", 1
	one, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatalf("open a one-connection pool as %s: %v", role, err)
	}
	t.Cleanup(one.Close)
	return one
}

func sessionCostDelay(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (int32, string) {
	t.Helper()
	var pid int32
	var delay string
	if err := pool.QueryRow(ctx, "SELECT pg_backend_pid(), "+
		"current_setting('vacuum_cost_delay')").Scan(&pid, &delay); err != nil {
		t.Fatalf("read the pooled session: %v", err)
	}
	return pid, delay
}
