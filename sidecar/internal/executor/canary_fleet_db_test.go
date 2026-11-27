package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rollout"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-04 integration: a fleet canary of a verified CREATE INDEX
// CONCURRENTLY across two real databases with real executors. The target
// records the action as "monitoring"; the canary waits for the target's
// own verdict before it judges, and completes once it is a success.

const fleetCanarySQL = "CREATE INDEX CONCURRENTLY idx_canary_orders " +
	"ON public.canary_orders (status)"

const fleetCanaryRollbackSQL = "DROP INDEX CONCURRENTLY public.idx_canary_orders"

// executorTarget is rollout.CanaryTarget over a real Executor, reading the
// same action log and verification rows as the sidecar's fleet adapter.
type executorTarget struct {
	name string
	e    *Executor
}

func (t executorTarget) Name() string { return t.name }

func (t executorTarget) SourceAction(ctx context.Context, id int64) (rollout.SourceAction,
	error) {
	var a rollout.SourceAction
	err := t.e.pool.QueryRow(ctx, `SELECT l.sql_executed, COALESCE(l.rollback_sql, ''),
		l.action_type, l.outcome, COALESCE(v.verdict, '')
		FROM sage.action_log l LEFT JOIN sage.verification v ON v.id = l.verification_id
		WHERE l.id = $1`, id).Scan(&a.SQL, &a.RollbackSQL, &a.ActionType, &a.Outcome,
		&a.Verification)
	return a, err
}

func (t executorTarget) MatchingFinding(ctx context.Context, sql string) (int, bool, error) {
	var id int
	err := t.e.pool.QueryRow(ctx, `SELECT id FROM sage.findings WHERE status = 'open'
		AND recommended_sql = $1 ORDER BY id DESC LIMIT 1`, sql).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (t executorTarget) Execute(ctx context.Context, findingID int, sql, rollbackSQL string,
	approvedBy *int) (int64, error) {
	return t.e.ExecuteManual(ctx, findingID, sql, rollbackSQL, approvedBy)
}

func (t executorTarget) Rollback(ctx context.Context, id int64, reason string) error {
	return t.e.RollbackAction(ctx, id, reason)
}

func (t executorTarget) ActionResult(ctx context.Context, id int64) (string, string, error) {
	a, err := t.SourceAction(ctx, id)
	return a.Outcome, a.Verification, err
}

func (t executorTarget) MonitorDeadline(ctx context.Context, id int64) (time.Time, error) {
	return t.e.MonitorDeadline(ctx, id)
}

// fleetDatabase is one bootstrapped database with a table, an open
// recommendation of fleetCanarySQL and an operator-mode executor whose
// monitor would not judge within the test (the test plays its verdict).
func fleetDatabase(t *testing.T, label string) executorTarget {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("config schema %s: %v", label, err)
	}
	_, err = pool.Exec(ctx, `CREATE TABLE public.canary_orders (id int, status text);
		INSERT INTO public.canary_orders SELECT g, 'open' FROM generate_series(1, 200) g`)
	if err != nil {
		t.Fatalf("table on %s: %v", label, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail, recommendation, recommended_sql, status)
		VALUES ('missing_index', 'warning', 'table', 'public.canary_orders', 'idx', '{}',
		        'rec', $1, 'open')`, fleetCanarySQL); err != nil {
		t.Fatalf("finding on %s: %v", label, err)
	}
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Trust.RollbackWindowMinutes = 60
	cfg.Verify.WindowMaxMinutes = 60
	var mu sync.Mutex
	e := New(pool, cfg, time.Now(), func(_, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		t.Logf("%s: %s", label, fmt.Sprintf(msg, args...))
	})
	withTestStandingGate(e)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		_ = e.Shutdown(sctx)
	})
	return executorTarget{name: label, e: e}
}

// settleVerdict records the monitor's verdict through the monitor's own
// settle path, as the rollback monitor does when its window ends.
func settleVerdict(t *testing.T, target executorTarget, actionID int64, verdict string) {
	t.Helper()
	ok := settleOutcome(context.Background(), target.e.pool, verify.Outcome{
		ActionLogID: actionID, Verdict: verdict, Reason: "integration verdict"},
		target.e.logFn)
	if !ok {
		t.Fatalf("settle action %d on %s: not monitorable", actionID, target.name)
	}
}

func latestAction(t *testing.T, target executorTarget) (int64, string) {
	t.Helper()
	var id int64
	var outcome string
	err := target.e.pool.QueryRow(context.Background(), `SELECT id, outcome
		FROM sage.action_log WHERE sql_executed = $1 ORDER BY id DESC LIMIT 1`,
		fleetCanarySQL).Scan(&id, &outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ""
	}
	if err != nil {
		t.Fatalf("read action on %s: %v", target.name, err)
	}
	return id, outcome
}

func TestFleetCanaryWaitsForTheTargetVerdictOnRealDatabases(t *testing.T) {
	controlPool, _ := requireDB(t)
	source := fleetDatabase(t, "canary_src")
	target := fleetDatabase(t, "canary_tgt")
	ctx := context.Background()
	srcID, err := source.e.ExecuteManual(ctx, firstFinding(t, source), fleetCanarySQL,
		fleetCanaryRollbackSQL, nil)
	if err != nil {
		t.Fatalf("source execute: %v", err)
	}
	settleVerdict(t, source, srcID, verify.OutcomeImproved)
	targets := map[string]rollout.CanaryTarget{"canary_src": source, "canary_tgt": target}
	svc, err := rollout.NewCanaryService(func(name string) (rollout.CanaryTarget, error) {
		if tg, ok := targets[name]; ok {
			return tg, nil
		}
		return nil, fmt.Errorf("database %q is not in the fleet", name)
	}, rollout.NewPostgresRunStore(controlPool), rollout.CanaryOptions{CanaryInstances: 1,
		RegressionLimitPct: 10, PollEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started, err := svc.Start(ctx, rollout.StartRequest{SourceDatabase: "canary_src",
		ActionLogID: srcID, Targets: []string{"canary_tgt"}, StartedBy: "user:1:a@example.com"})
	if err != nil {
		t.Fatalf("start canary: %v", err)
	}
	actionID := waitForMonitoring(t, target)
	time.Sleep(200 * time.Millisecond) // ten polls of a still-monitoring verdict
	if run, _, err := svc.Get(ctx, started.EvidenceID); err != nil || run.State != "canary" {
		t.Fatalf("run judged while the target was still monitoring: %+v (%v)", run, err)
	}
	settleVerdict(t, target, actionID, verify.OutcomeImproved)
	run := waitForRunEnd(t, svc, started.EvidenceID)
	if run.State != "complete" || run.AppliedInstances != 1 {
		t.Fatalf("run = %+v, want complete (CHECK-01)", run)
	}
	if _, outcome := latestAction(t, target); outcome != "success" {
		t.Fatalf("target action outcome %q, want success (not rolled back)", outcome)
	}
}

func firstFinding(t *testing.T, target executorTarget) int {
	t.Helper()
	id, found, err := target.MatchingFinding(context.Background(), fleetCanarySQL)
	if err != nil || !found {
		t.Fatalf("finding on %s: %v %v", target.name, found, err)
	}
	return id
}

func waitForMonitoring(t *testing.T, target executorTarget) int64 {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if id, outcome := latestAction(t, target); id > 0 {
			if outcome != "monitoring" {
				t.Fatalf("target action %d outcome %q, want monitoring", id, outcome)
			}
			return id
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the canary never executed on the target")
	return 0
}

func waitForRunEnd(t *testing.T, svc *rollout.CanaryService, id string) rollout.RunRecord {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		run, _, err := svc.Get(context.Background(), id)
		if err == nil && run.State != "canary" {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the canary never finished")
	return rollout.RunRecord{}
}
