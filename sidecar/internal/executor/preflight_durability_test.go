package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/verify"
)

// These pre-remediation tests intentionally assert the desired recovery contract.
// Only the authorization verdict is stubbed; evidence, durable state, DDL and
// backend termination use the disposable PostgreSQL fixture.
type preflightAllowGate struct{}

func (preflightAllowGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	return policy.Decision{Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe}
}

type preflightFixture struct {
	pool   *pgxpool.Pool
	at     time.Time
	id     int64
	watch  verifiedIndexAction
	engine *verify.Engine
	store  *verify.PostgresStateStore
}

func preflightPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func preflightNewFixture(t *testing.T) *preflightFixture {
	t.Helper()
	f := &preflightFixture{pool: preflightPool(t), at: time.Now().UTC().Add(-2 * time.Minute)}
	_, err := f.pool.Exec(t.Context(), `CREATE TABLE public.preflight_items (id int);
		CREATE INDEX preflight_items_idx ON public.preflight_items(id)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cleanup, err := pgxpool.New(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"))
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(ctx, `DROP TABLE public.preflight_items`); err != nil {
			t.Errorf("cleanup table: %v", err)
		}
	})
	var decisionID int64
	err = f.pool.QueryRow(t.Context(), `INSERT INTO sage.decision
		(feature,intent,verdict,risk_tier,reason,evidence_id)
		VALUES ('index','index','execute','safe','preflight',$1) RETURNING id`,
		fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())).Scan(&decisionID)
	if err != nil {
		t.Fatal(err)
	}
	err = f.pool.QueryRow(t.Context(), `INSERT INTO sage.action_log
		(action_type,sql_executed,rollback_sql,decision_id,outcome,before_state)
		VALUES ('create_index',$1,$2,$3,'pending',$4) RETURNING id`,
		"CREATE INDEX preflight_items_idx ON public.preflight_items(id)",
		"DROP INDEX CONCURRENTLY IF EXISTS public.preflight_items_idx", decisionID,
		preflightCreatedIdentity(t, f.pool)).Scan(&f.id)
	if err != nil {
		t.Fatal(err)
	}
	f.watch = verifiedIndexAction{WatchID: t.Name(), Table: "public.preflight_items",
		IndexName: "public.preflight_items_idx", QueryIDs: []int64{f.id},
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS public.preflight_items_idx",
		Criterion: verify.Criterion{Kind: "per_query_latency", TargetIDs: []int64{f.id},
			Window: time.Minute, HardMax: 4 * time.Minute}}
	preflightSeedMeasurements(t, f)
	f.store = verify.NewPostgresStateStore(f.pool, 1)
	f.engine = preflightEngine(t, f, f.store)
	return f
}

// preflightCreatedIdentity records the created index's OID identity through
// the production path (recordCreatedIndexIdentity on the schema-qualified
// name prepareVerifiedIndex produces), exactly as executeFinding stores it in
// action_log.before_state. revert_created_index refuses to drop without it.
func preflightCreatedIdentity(t *testing.T, pool *pgxpool.Pool) []byte {
	t.Helper()
	e := New(pool, &config.Config{}, nil, time.Time{}, func(string, string, ...any) {})
	state := map[string]any{}
	qualified := pgx.Identifier{"public", "preflight_items_idx"}.Sanitize()
	e.recordCreatedIndexIdentity(t.Context(), qualified, state)
	if state["created_index_oid"] == nil {
		t.Fatalf("created index identity for %s was not recorded", qualified)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// preflightRecoveryEngine models a restarted process whose clock has passed
// the durable claim lease (verify.RevertRetryInterval) the crashed or failed
// owner left on the watch. Before that lease expires the owner may still be
// alive, so recovery must not act (see TestPreflightDurableRevertCrashBeforeEffect).
func preflightRecoveryEngine(
	t *testing.T, f *preflightFixture, store verify.StateStore,
) *verify.Engine {
	t.Helper()
	opts := verify.DefaultOptions()
	opts.MinSamples = 1
	recoverAt := f.at.Add(2*time.Minute + verify.RevertRetryInterval + time.Second)
	opts.Now = func() time.Time { return recoverAt }
	engine, err := verify.NewEngine(verify.NewPostgresObservationSource(f.pool), store, opts)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func preflightSeedMeasurements(t *testing.T, f *preflightFixture) {
	t.Helper()
	for _, sample := range []struct {
		delay time.Duration
		calls int
		ms    float64
	}{{-50 * time.Second, 0, 0}, {-time.Second, 100, 100},
		{time.Second, 100, 100}, {50 * time.Second, 200, 400}} {
		_, err := f.pool.Exec(t.Context(), `INSERT INTO sage.query_store
			(captured_at,queryid,calls,total_exec_time,mean_exec_time)
			VALUES ($1,$2,$3,$4,1)`, f.at.Add(sample.delay), f.id, sample.calls, sample.ms)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func preflightEngine(t *testing.T, f *preflightFixture, store verify.StateStore) *verify.Engine {
	t.Helper()
	opts := verify.DefaultOptions()
	opts.MinSamples = 1
	opts.Now = func() time.Time { return f.at.Add(2 * time.Minute) }
	engine, err := verify.NewEngine(verify.NewPostgresObservationSource(f.pool), store, opts)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func preflightActions(f *preflightFixture) *executorIndexActions {
	e := New(f.pool, &config.Config{}, nil, time.Time{}, func(string, string, ...any) {})
	e.WithPolicyGate(preflightAllowGate{})
	return &executorIndexActions{exec: e}
}

func preflightLifecycle(f *preflightFixture, engine *verify.Engine) *verifiedIndexLifecycle {
	actions := preflightActions(f)
	l := newVerifiedIndexLifecycle(&postgresIndexVerifier{engine: engine, exec: actions.exec}, actions)
	l.now = func() time.Time { return f.at }
	return l
}

func preflightAssertRecovered(t *testing.T, f *preflightFixture) {
	t.Helper()
	var exists bool
	var verdict, outcome string
	var completed bool
	err := f.pool.QueryRow(t.Context(), `SELECT
		to_regclass('public.preflight_items_idx') IS NOT NULL,
		v.verdict, v.completed_at IS NOT NULL, al.outcome
		FROM sage.verification v JOIN sage.action_log al ON al.id=v.action_log_id
		WHERE al.id=$1`, f.id).Scan(&exists, &verdict, &completed, &outcome)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("durable state: index_exists=%v verdict=%s completed=%v outcome=%s",
		exists, verdict, completed, outcome)
	if exists || outcome != "rolled_back" {
		t.Fatalf("recovery lost required rollback: index_exists=%v outcome=%s", exists, outcome)
	}
}

func TestPreflightDurableRevertHappyPath(t *testing.T) {
	f := preflightNewFixture(t)
	if err := preflightLifecycle(f, f.engine).WatchApplied(t.Context(), f.watch, f.id); err != nil {
		t.Fatal(err)
	}
	preflightAssertRecovered(t, f)
}

func TestPreflightDurableRevertCrashBeforeEffect(t *testing.T) {
	f := preflightNewFixture(t)
	// Stop at the exact persisted-decision / not-yet-run-effect crash boundary.
	v, err := f.engine.Watch(t.Context(), watchRequest(f.watch, f.id, f.at))
	if err != nil || !v.Revert || v.Reason != "query_regression" {
		t.Fatalf("regression verdict=%+v err=%v", v, err)
	}
	// Reconstruct every in-memory component and reopen the pool from durable state.
	f.pool.Close()
	f.pool = preflightPool(t)
	// At the crash instant the owner's claim lease is still live: no second
	// worker may take the revert yet, and nothing may be lost either.
	early := preflightEngine(t, f, verify.NewPostgresStateStore(f.pool, 1))
	if err := preflightLifecycle(f, early).ResumeDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	preflightAssertRevertStillOwed(t, f)
	fresh := preflightRecoveryEngine(t, f, verify.NewPostgresStateStore(f.pool, 1))
	if err := preflightLifecycle(f, fresh).ResumeDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	preflightAssertRecovered(t, f)
}

func preflightAssertRevertStillOwed(t *testing.T, f *preflightFixture) {
	t.Helper()
	var exists, completed bool
	var verdict string
	err := f.pool.QueryRow(t.Context(), `SELECT
		to_regclass('public.preflight_items_idx') IS NOT NULL,
		v.verdict, v.completed_at IS NOT NULL
		FROM sage.verification v WHERE v.action_log_id=$1`, f.id).
		Scan(&exists, &verdict, &completed)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || verdict != "revert" || completed {
		t.Fatalf("revert intent not durably owed: index_exists=%v verdict=%s completed=%v",
			exists, verdict, completed)
	}
}

func TestPreflightDurableRevertConnectionLossThenRestart(t *testing.T) {
	f := preflightNewFixture(t)
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(t.Context(),
		"LOCK TABLE public.preflight_items IN ACCESS EXCLUSIVE MODE",
	); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- preflightLifecycle(f, f.engine).WatchApplied(t.Context(), f.watch, f.id)
	}()
	pid := preflightWaitForDrop(t, f.pool)
	var killed bool
	err = f.pool.QueryRow(t.Context(), "SELECT pg_terminate_backend($1)", pid).Scan(&killed)
	if err != nil || !killed {
		t.Fatalf("terminate exact blocked DDL backend pid=%d: killed=%v err=%v", pid, killed, err)
	}
	err = <-done
	if err == nil || !strings.Contains(err.Error(), "revert verified index") {
		t.Fatalf("connection-loss error not propagated: %v", err)
	}
	t.Logf("actual backend connection-loss error: %v", err)
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	fresh := preflightRecoveryEngine(t, f, verify.NewPostgresStateStore(f.pool, 1))
	if err := preflightLifecycle(f, fresh).ResumeDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	preflightAssertRecovered(t, f)
}

func preflightWaitForDrop(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		err := pool.QueryRow(t.Context(), `SELECT pid FROM pg_stat_activity
			WHERE datname=current_database() AND pid<>pg_backend_pid()
			AND state='active' AND wait_event_type='Lock'
			AND query LIKE 'DROP INDEX CONCURRENTLY%' LIMIT 1`).Scan(&pid)
		if err == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("real DROP never reached lock wait; fault injection was not exercised")
	return 0
}

type preflightBarrierStore struct {
	*verify.PostgresStateStore
	ready chan<- struct{}
	goOn  <-chan struct{}
}

func (s preflightBarrierStore) ListDue(
	ctx context.Context, now time.Time,
) ([]verify.WatchState, error) {
	states, err := s.PostgresStateStore.ListDue(ctx, now)
	s.ready <- struct{}{}
	select {
	case <-s.goOn:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return states, err
}

type preflightCountActions struct {
	*executorIndexActions
	reverts *atomic.Int32
}

func (a preflightCountActions) Revert(
	ctx context.Context, id int64, sql string, v verify.Verdict,
) error {
	a.reverts.Add(1)
	return a.executorIndexActions.Revert(ctx, id, sql, v)
}

func TestPreflightConcurrentRecoveryClaimsEachWatchOnce(t *testing.T) {
	f := preflightNewFixture(t)
	state := verify.WatchStateFromRequest(watchRequest(f.watch, f.id, f.at))
	if err := f.store.Create(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	ready, goOn := make(chan struct{}, 2), make(chan struct{})
	var reverts atomic.Int32
	errs := make(chan error, 2)
	for range 2 {
		store := preflightBarrierStore{verify.NewPostgresStateStore(f.pool, 1), ready, goOn}
		engine := preflightEngine(t, f, store)
		actions := preflightCountActions{preflightActions(f), &reverts}
		lifecycle := newVerifiedIndexLifecycle(
			&postgresIndexVerifier{engine: engine, exec: actions.exec}, actions)
		go func() { errs <- lifecycle.ResumeDue(t.Context()) }()
	}
	<-ready
	<-ready
	close(goOn)
	for range 2 {
		if err := <-errs; err != nil {
			t.Logf("concurrent finalization error: %v", err)
		}
	}
	preflightAssertRecovered(t, f)
	if got := reverts.Load(); got != 1 {
		t.Fatal(fmt.Sprintf("one durable watch finalized %d times; want a single claim", got))
	}
}
