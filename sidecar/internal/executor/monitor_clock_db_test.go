package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-31 on real PostgreSQL. A database whose clock leads the
// sidecar host's is the real server clock moved dbLead ahead (the test
// cannot move the server's clock): samples and executed_at are stamped on
// that clock, as DEFAULT now() stamps them on a managed platform.

const dbLead = 10 * time.Minute

// leadingClock is pool's server clock moved lead ahead of the host.
func leadingClock(pool *pgxpool.Pool, lead time.Duration) dbClock {
	return func(ctx context.Context) (time.Time, error) {
		at, err := testdb.DBNow(ctx, pool)
		return at.Add(lead), err
	}
}

func clockMonitorConfig(rec *recordedRollback, clock dbClock) RollbackMonitorConfig {
	return RollbackMonitorConfig{ThresholdPct: 10, WindowMinutes: 15, CapMinutes: 4320,
		StatementTimeout: time.Minute, LockTimeoutMs: 1500, Authorize: allowRollback,
		execRollback: rec.exec, clock: clock}
}

// clockAction is a monitored CREATE INDEX executed at executedAt whose
// prediction targets one fresh queryid with a frozen 10 ms baseline.
func clockAction(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	executedAt time.Time) (id, qid int64, rollback string) {
	t.Helper()
	qid = int64(8_770_000_000) + time.Now().UnixNano()%1_000_000
	p := rulePrediction(-30)
	p.Class, p.Metric, p.TargetQueryIDs = verify.ClassIndexCreate,
		verify.MetricMeanExecTime, []int64{qid}
	name := fmt.Sprintf("clk_%d", qid)
	rollback = "DROP INDEX CONCURRENTLY IF EXISTS public." + name
	id = insertVerifiedAction(t, pool, verifiedActionRow{
		sql:      "CREATE INDEX CONCURRENTLY " + name + " ON public.t (a)",
		rollback: rollback, before: beforeStateJSON(p, fixtureBaselineMs, nil),
		executedAt: executedAt.UTC(), decisionID: insertParkDecision(t, ctx, pool)})
	return id, qid, rollback
}

// afterCalls is the calls the verdict counted after the action.
func afterCalls(t *testing.T, o verify.Outcome) float64 {
	t.Helper()
	cmp, _ := o.Evidence["comparison"].(map[string]any)
	after, _ := cmp["after"].(map[string]any)
	calls, ok := after["calls"].(float64)
	if !ok {
		t.Fatalf("outcome evidence has no after-window calls: %v", o.Evidence)
	}
	return calls
}

func clockEvidence(t *testing.T, o verify.Outcome) map[string]any {
	t.Helper()
	ev, ok := o.Evidence["clock"].(map[string]any)
	if !ok {
		t.Fatalf("outcome evidence does not name the window's clock: %v", o.Evidence)
	}
	return ev
}

func TestPoolClock_ReadsTheServerClock(t *testing.T) {
	pool, ctx := requireDB(t)
	before := testdb.MustDBNow(t, pool)
	got, err := poolClock(pool)(ctx)
	after := testdb.MustDBNow(t, pool)
	if err != nil {
		t.Fatalf("poolClock: %v", err)
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("poolClock = %s, outside the server clock's [%s, %s]", got, before, after)
	}
}

func TestPoolClock_CancelledContextIsAnError(t *testing.T) {
	pool, _ := requireDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	at, err := poolClock(pool)(ctx)
	if !errors.Is(err, context.Canceled) || !at.IsZero() {
		t.Fatalf("poolClock(cancelled) = %s, %v; want context.Canceled and no time", at, err)
	}
	if !strings.Contains(err.Error(), "clock_timestamp()") {
		t.Fatalf("error %q does not name the clock read", err)
	}
}

// CHECK-01: with the database clock ahead of the host, the window ends at
// the database clock, so the newest samples count.
func TestMonitor_WindowEndsAtTheDatabaseClock(t *testing.T) {
	pool, base := requireDB(t)
	ctx, cancel := context.WithTimeout(base, time.Minute)
	defer cancel()
	dbNow := testdb.MustDBNow(t, pool)
	executedAt := dbNow.Add(-16 * time.Minute) // first check due on either clock
	id, qid, rollback := clockAction(t, ctx, pool, executedAt)
	// 25 minutes of samples: the newest nine are stamped after the host's now.
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 25, 40,
		fixtureBaselineMs/2)
	rec := &recordedRollback{}

	MonitorAndRollback(ctx, pool, id, rollback,
		clockMonitorConfig(rec, leadingClock(pool, dbLead)), nopLog, nil)

	got := storedOutcome(t, pool, id)
	lead := dbNow.Add(dbLead)
	if got.WindowEnd == nil || got.WindowEnd.Before(lead) {
		t.Fatalf("window ended at %v, before the database clock %s: bounded by the host's",
			got.WindowEnd, lead)
	}
	if calls := afterCalls(t, got); calls != 25*40 {
		t.Fatalf("after-window calls = %.0f, want 1000: the samples stamped after the "+
			"host's now were dropped", calls)
	}
	if got.Verdict != verify.OutcomeImproved || rec.calls != 0 {
		t.Fatalf("outcome = %s (%s), rollbacks = %d; want improved, kept", got.Verdict,
			got.Reason, rec.calls)
	}
	if ev := clockEvidence(t, got); ev["source"] != clockSourceDatabase {
		t.Fatalf("clock evidence = %v, want the database clock", ev)
	}
}

// The first check is due by the database clock (16 minutes after the
// action) although the host's clock is nine minutes short of it: the
// monitor must not wait those nine minutes.
func TestMonitor_WaitsOnTheDatabaseClock(t *testing.T) {
	pool, base := requireDB(t)
	ctx, cancel := context.WithTimeout(base, 45*time.Second)
	defer cancel()
	dbNow := testdb.MustDBNow(t, pool)
	executedAt := dbNow.Add(dbLead - 16*time.Minute)
	id, qid, rollback := clockAction(t, ctx, pool, executedAt)
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 15, 40,
		fixtureBaselineMs/2)
	rec := &recordedRollback{}
	start := time.Now()

	MonitorAndRollback(ctx, pool, id, rollback,
		clockMonitorConfig(rec, leadingClock(pool, dbLead)), nopLog, nil)

	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "success" {
		t.Fatalf("action outcome = %q after %s, want success: the monitor waited for the "+
			"host clock to reach a check the database clock had already reached",
			outcome, time.Since(start).Round(time.Second))
	}
	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeImproved || afterCalls(t, got) != 15*40 {
		t.Fatalf("outcome = %s (%s), want improved over all 600 calls", got.Verdict,
			got.Reason)
	}
}

// A failed database clock read judges by the host clock, says so in the
// verdict's evidence and the log, and still decides.
func TestMonitor_FallsBackToTheHostClockWithAReason(t *testing.T) {
	pool, base := requireDB(t)
	ctx, cancel := context.WithTimeout(base, time.Minute)
	defer cancel()
	dbNow := testdb.MustDBNow(t, pool)
	executedAt := dbNow.Add(-16 * time.Minute)
	id, qid, rollback := clockAction(t, ctx, pool, executedAt)
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 14, 40,
		fixtureBaselineMs/2)
	rec := &recordedRollback{}
	logs := &clockLog{}
	hostBefore := time.Now()

	MonitorAndRollback(ctx, pool, id, rollback, clockMonitorConfig(rec,
		failingClock(errors.New("database clock unreachable (test)"))), logs.log, nil)

	hostAfter := time.Now()
	got := storedOutcome(t, pool, id)
	if got.Verdict != verify.OutcomeImproved {
		t.Fatalf("outcome = %s (%s), want improved on the host clock", got.Verdict,
			got.Reason)
	}
	if got.WindowEnd == nil || got.WindowEnd.Before(hostBefore.Add(-time.Millisecond)) ||
		got.WindowEnd.After(hostAfter) {
		t.Fatalf("window end %v outside the host's [%s, %s]", got.WindowEnd, hostBefore,
			hostAfter)
	}
	ev := clockEvidence(t, got)
	reason, _ := ev["reason"].(string)
	if ev["source"] != clockSourceHost || !strings.Contains(reason, "unreachable (test)") {
		t.Fatalf("clock evidence = %v, want the host clock with the failed read's reason", ev)
	}
	if line := logs.joined(); !strings.Contains(line, "unreachable (test)") {
		t.Fatalf("the fallback was not logged: %q", line)
	}
}

// resumeAt is an executed action whose resume expiry (window + grace) is
// offset from the database's now.
func resumeAt(t *testing.T, exec *Executor, sql string, fromExpiry time.Duration) time.Time {
	t.Helper()
	_, capW, _ := monitorWindows(verificationClass(sql), exec.actionRollbackConfig(0, sql))
	grace := max(capW, minResumeGrace)
	return testdb.MustDBNow(t, exec.pool).Add(-(capW + grace) + fromExpiry)
}

func waitMonitorSettled(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		outcome, _ := actionOutcomeFor(t, pool, id)
		if outcome != "monitoring" || time.Now().After(deadline) {
			return outcome
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func shutdownOnCleanup(t *testing.T, exec *Executor) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.Shutdown(ctx)
	})
}

// A restarted sidecar expires a monitor by the database clock: an hour
// left on the host's clock is an hour past expiry on a database two hours
// ahead.
func TestResumeMonitor_ExpiresByTheDatabaseClock(t *testing.T) {
	_, exec, ctx := verifiedTable(t, "public")
	shutdownOnCleanup(t, exec)
	exec.clock = leadingClock(exec.pool, 2*time.Hour)
	probe := "CREATE INDEX CONCURRENTLY clk_probe ON public.t (a)"
	executedAt := resumeAt(t, exec, probe, time.Hour)
	id, _, rollback := clockAction(t, ctx, exec.pool, executedAt)
	sql := actionSQL(t, exec.pool, id)

	exec.resumeMonitor(ctx, id, rollback, sql, executedAt)

	if outcome := waitMonitorSettled(t, exec.pool, id); outcome != "verification_expired" {
		t.Fatalf("action outcome = %q, want verification_expired by the database clock "+
			"(the host's clock still had an hour)", outcome)
	}
}

// State transition the other way: a database two hours behind the host
// still has an hour of grace, so the restarted sidecar judges the action
// instead of expiring it.
func TestResumeMonitor_DatabaseClockBehindKeepsWatching(t *testing.T) {
	_, exec, ctx := verifiedTable(t, "public")
	shutdownOnCleanup(t, exec)
	exec.clock = leadingClock(exec.pool, -2*time.Hour)
	probe := "CREATE INDEX CONCURRENTLY clk_probe ON public.t (a)"
	// The host's clock passed expiry an hour ago; the lagging database
	// clock is an hour short of it.
	executedAt := resumeAt(t, exec, probe, -time.Hour)
	id, _, rollback := clockAction(t, ctx, exec.pool, executedAt)
	sql := actionSQL(t, exec.pool, id)

	exec.resumeMonitor(ctx, id, rollback, sql, executedAt)

	outcome := waitMonitorSettled(t, exec.pool, id)
	if outcome == "verification_expired" || outcome == "monitoring" {
		t.Fatalf("action outcome = %q, want judged at its cap (the database clock had an "+
			"hour of grace left)", outcome)
	}
	if got := storedOutcome(t, exec.pool, id); got.Verdict != verify.OutcomeInsufficient {
		t.Fatalf("outcome = %s (%s), want insufficient_evidence (no samples)", got.Verdict,
			got.Reason)
	}
}

func actionSQL(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var sql string
	if err := pool.QueryRow(context.Background(),
		"SELECT sql_executed FROM sage.action_log WHERE id = $1", id).Scan(&sql); err != nil {
		t.Fatalf("read action %d: %v", id, err)
	}
	return sql
}

// The frozen pre-action baseline ends at the database clock: samples the
// database stamped after the host's now are part of it.
func TestFreezeBaseline_IncludesSamplesStampedAheadOfTheHost(t *testing.T) {
	_, exec, ctx := verifiedTable(t, "public")
	exec.clock = leadingClock(exec.pool, dbLead)
	qid := int64(8_780_000_000) + time.Now().UnixNano()%1_000_000
	dbNow := testdb.MustDBNow(t, exec.pool)
	seedQuerySamples(t, ctx, exec.pool, qid, dbNow.Add(time.Minute), 8, 40, fixtureBaselineMs)

	got := exec.freezeBaseline(ctx, verify.ClassIndexCreate, []int64{qid})

	m, ok := got[fmt.Sprint(qid)]
	if !ok || m.Samples != 8*40 {
		t.Fatalf("frozen baseline = %+v, want %d calls for %d: samples stamped after the "+
			"host's now were dropped", got, 8*40, qid)
	}
}

// The statistics estimate baseline ends at the database clock too.
func TestStatisticsBaseline_IncludesPlansStampedAheadOfTheHost(t *testing.T) {
	fx := newStatsFixture(t)
	fx.exec.clock = leadingClock(fx.pool, dbLead)
	fx.capturePlans(t, testdb.MustDBNow(t, fx.pool).Add(5*time.Minute))
	p := verify.Prediction{TargetQueryIDs: []int64{fx.qid}}
	before := map[string]any{}

	fx.exec.statisticsBaseline(fx.ctx, fx.sql, &p, before)

	sample, ok := before["estimate_baseline"].(verify.EstimateSample)
	if !ok || sample.Plans != 3 {
		t.Fatalf("estimate baseline = %#v, want the 3 plans stamped after the host's now",
			before["estimate_baseline"])
	}
}
