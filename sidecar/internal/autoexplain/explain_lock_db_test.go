package autoexplain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rca-05: the on-demand EXPLAIN gives up on a busy relation lock after
// selfmonitor.ProbeLockTimeout instead of queueing behind DDL for the
// whole statement_timeout. No parallel runs: the tests share the package's
// fixture database and its explain_lock_t table.

const lockTable = "explain_lock_t"

// lockFixture is a one-connection collector pool (so a temporary
// pg_stat_statements shadows the view for Collect, and SHOW reads the
// session the EXPLAIN ran on) and a separate lock holder.
type lockFixture struct {
	pool   *pgxpool.Pool
	holder *pgx.Conn
	rec    *testdb.QueryRecorder
	mu     sync.Mutex
	lines  []logLine
}

func newLockFixture(t *testing.T) *lockFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SAGE_DATABASE_URL"))
	if dsn == "" || strings.Contains(dsn, "pgsage_test_disabled") {
		t.Skip("SKIPPED: SAGE_DATABASE_URL fixture is not configured")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	f := &lockFixture{rec: &testdb.QueryRecorder{}}
	cfg.ConnConfig.Tracer = f.rec
	if f.pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.pool.Close)
	bootstrapSageSchema(t, f.pool)
	if f.holder, err = pgx.Connect(ctx, dsn); err != nil {
		t.Fatalf("connect lock holder: %v", err)
	}
	t.Cleanup(func() { _ = f.holder.Close(context.Background()) })
	_, err = f.holder.Exec(ctx, `DROP TABLE IF EXISTS `+lockTable+`;
		CREATE TABLE `+lockTable+` (id int PRIMARY KEY, v text);
		INSERT INTO `+lockTable+` SELECT g, 'v' || g FROM generate_series(1, 100) g`)
	if err != nil {
		t.Fatalf("create lock fixture table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.holder.Exec(context.Background(), "DROP TABLE IF EXISTS "+lockTable)
	})
	f.rec.Reset()
	return f
}

// collector reads the fixture's candidates: pg_temp.pg_stat_statements,
// which the qualified reader (security-01c-q) no longer finds by
// search_path.
func (f *lockFixture) collector() *Collector {
	c := NewCollector(f.pool,
		CollectorConfig{CollectIntervalSeconds: 60, MaxPlansPerCycle: 10},
		&Availability{Available: false, Method: "unavailable"},
		func(level, format string, args ...any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lines = append(f.lines, logLine{level, fmt.Sprintf(format, args...)})
		})
	c.statementsRel = "pg_temp.pg_stat_statements"
	return c
}

func (f *lockFixture) logs(level string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.lines {
		if l.level == level {
			out = append(out, l.msg)
		}
	}
	return out
}

// lockExclusive opens a transaction on the holder that takes ACCESS
// EXCLUSIVE on lockTable, as a running ALTER TABLE would.
func (f *lockFixture) lockExclusive(t *testing.T) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := f.holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, "LOCK TABLE "+lockTable+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("take ACCESS EXCLUSIVE: %v", err)
	}
	return tx
}

func (f *lockFixture) storedPlans(t *testing.T, queryIDs ...int64) map[int64]string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT queryid, source
		FROM sage.explain_cache WHERE queryid = ANY($1)`, queryIDs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct {
		id  int64
		src string
	}, error) {
		var v struct {
			id  int64
			src string
		}
		return v, r.Scan(&v.id, &v.src)
	})
	if err != nil {
		t.Fatal(err)
	}
	m := map[int64]string{}
	for _, v := range out {
		m[v.id] = v.src
	}
	return m
}

func (f *lockFixture) cleanPlans(t *testing.T, queryIDs ...int64) {
	t.Helper()
	clean := func() {
		_, _ = f.pool.Exec(context.Background(),
			"DELETE FROM sage.explain_cache WHERE queryid = ANY($1)", queryIDs)
	}
	clean()
	t.Cleanup(clean)
}

// assertBoundsRolledBack proves the SET LOCAL bounds ended with the
// EXPLAIN transaction instead of leaking into the pooled session.
func (f *lockFixture) assertBoundsRolledBack(t *testing.T) {
	t.Helper()
	for _, guc := range []string{"lock_timeout", "idle_in_transaction_session_timeout"} {
		var v string
		if err := f.pool.QueryRow(context.Background(), "SHOW "+guc).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != "0" {
			t.Fatalf("%s leaked into the pooled session: %q", guc, v)
		}
	}
}

func TestExplainLockBusyReturnsWithinOneSecond(t *testing.T) {
	f := newLockFixture(t)
	const qid = int64(991200501)
	f.cleanPlans(t, qid)
	f.lockExclusive(t)

	start := time.Now()
	err := f.collector().captureOnDemand(context.Background(), qid,
		"SELECT * FROM "+lockTable+" WHERE id = 1")
	elapsed := time.Since(start)

	if !errors.Is(err, errLockBusy) {
		t.Fatalf("capture behind ACCESS EXCLUSIVE = %v, want errLockBusy", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("errLockBusy must keep the 55P03 server error, got %v", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("capture waited %v behind the lock, want under 1s", elapsed)
	}
	if elapsed < 90*time.Millisecond {
		t.Fatalf("capture returned in %v, before the 100ms lock timeout", elapsed)
	}
	if got := f.storedPlans(t, qid); len(got) != 0 {
		t.Fatalf("a lock-busy capture must store nothing, got %v", got)
	}
	f.assertBoundsRolledBack(t)
}

// rca-04: the capture is a plain EXPLAIN, so its source is explain_estimate
// (it was mislabelled auto_explain).
func TestExplainWithoutLockStoresAnEstimate(t *testing.T) {
	f := newLockFixture(t)
	const qid = int64(991200502)
	f.cleanPlans(t, qid)

	err := f.collector().captureOnDemand(context.Background(), qid,
		"SELECT * FROM "+lockTable+" WHERE id = 1")
	if err != nil {
		t.Fatalf("capture without a lock: %v", err)
	}
	if got := f.storedPlans(t, qid); got[qid] != estimateSource {
		t.Fatalf("stored plans = %v, want source %s", got, estimateSource)
	}
	for _, sql := range []string{"SET TRANSACTION READ ONLY",
		"SET LOCAL statement_timeout = '5s'",
		"SET LOCAL lock_timeout = '100ms'",
		"SET LOCAL idle_in_transaction_session_timeout = '10s'"} {
		if n := len(f.rec.Matching(sql)); n != 1 {
			t.Errorf("%q sent %d times, want once", sql, n)
		}
	}
	f.assertBoundsRolledBack(t)
}

// Boundary: a lock wait shorter than the lock timeout still captures. The
// holder releases the lock from the server, as soon as the EXPLAIN waits
// (releaseOnWaiterSQL), well inside the 100ms timeout. A release from the
// client after a ~50ms hold lost 4 of 50 runs alone on PG17, and one that
// polled pg_locks from the client lost under load (released 212-270ms
// after the capture started; ci-17). Only a lock-busy attempt is retried
// (three at most); any other outcome decides the test at once.
func TestExplainLockReleasedWithinTimeoutStillCaptures(t *testing.T) {
	f := newLockFixture(t)
	const qid = int64(991200503)
	f.cleanPlans(t, qid)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = f.captureAfterShortLock(t, qid); !errors.Is(err, errLockBusy) {
			break
		}
		t.Logf("attempt %d: %v", attempt+1, err)
	}
	if err != nil {
		t.Fatalf("capture after a short lock wait: %v", err)
	}
	if got := f.storedPlans(t, qid); got[qid] != estimateSource {
		t.Fatalf("stored plans = %v, want source %s", got, estimateSource)
	}
}

// releaseOnWaiterSQL runs on the holder inside its lock transaction: it
// polls pg_locks on the server every millisecond (5 s at most) until a
// session waits for lockTable, notes that in a session setting, and
// commits in the same round trip, so no client latency delays the release.
const releaseOnWaiterSQL = `DO $$
BEGIN
	PERFORM pg_catalog.set_config('pg_sage_test.saw_waiter', 'off', false);
	FOR i IN 1..5000 LOOP
		IF EXISTS (SELECT 1 FROM pg_catalog.pg_locks
		           WHERE relation = '` + lockTable + `'::regclass AND NOT granted) THEN
			PERFORM pg_catalog.set_config('pg_sage_test.saw_waiter', 'on', false);
			EXIT;
		END IF;
		PERFORM pg_catalog.pg_sleep(0.001);
	END LOOP;
END $$; COMMIT`

// captureAfterShortLock runs one capture behind ACCESS EXCLUSIVE, which
// the holder releases from the server once the EXPLAIN waits for it.
func (f *lockFixture) captureAfterShortLock(t *testing.T, qid int64) error {
	t.Helper()
	ctx := context.Background()
	if _, err := f.holder.Exec(ctx, "BEGIN; LOCK TABLE "+lockTable+
		" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("take ACCESS EXCLUSIVE: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.collector().captureOnDemand(ctx, qid,
			"SELECT * FROM "+lockTable+" WHERE id = 1")
	}()
	if _, err := f.holder.Exec(ctx, releaseOnWaiterSQL); err != nil {
		_, _ = f.holder.Exec(ctx, "ROLLBACK")
		t.Fatalf("release the lock once the EXPLAIN waits: %v", err)
	}
	err := <-done
	var waited string
	if qerr := f.holder.QueryRow(ctx, `SELECT pg_catalog.current_setting(
		'pg_sage_test.saw_waiter')`).Scan(&waited); qerr != nil {
		t.Fatalf("read whether the EXPLAIN waited: %v", qerr)
	}
	if waited != "on" {
		t.Fatalf("the EXPLAIN never waited for ACCESS EXCLUSIVE (capture: %v)", err)
	}
	return err
}

// shadowCandidates makes Collect see exactly these statements, through a
// temporary pg_stat_statements on the collector's only session.
func (f *lockFixture) shadowCandidates(t *testing.T, cands map[int64]string) {
	t.Helper()
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `CREATE TEMP TABLE pg_stat_statements (
		queryid bigint, query text, mean_exec_time double precision,
		calls bigint, dbid oid)`)
	if err != nil {
		t.Fatal(err)
	}
	for id, q := range cands {
		_, err := f.pool.Exec(ctx, `INSERT INTO pg_stat_statements
			SELECT $1, $2, 500, 20, oid FROM pg_database
			WHERE datname = current_database()`, id, q)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Concurrency: two candidates behind one DDL lock both skip, each after one
// lock timeout, and the cycle reports them once at INFO. The cycle takes at
// least the two lock timeouts and stays far below one 5s statement_timeout
// (10s unbounded); the bound leaves room for the per-candidate round trips,
// which overran a 300ms bound on a loaded host.
func TestCollectSkipsCandidatesBehindOneLock(t *testing.T) {
	f := newLockFixture(t)
	ids := []int64{991200504, 991200505}
	f.cleanPlans(t, ids...)
	f.shadowCandidates(t, map[int64]string{
		ids[0]: "SELECT * FROM " + lockTable + " WHERE id = 1",
		ids[1]: "SELECT v FROM " + lockTable + " WHERE id = 2",
	})
	f.lockExclusive(t)

	start := time.Now()
	if err := f.collector().Collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 2*selfmonitor.ProbeLockTimeout || elapsed >= time.Second {
		t.Fatalf("cycle took %v behind one lock, want 2 lock timeouts (200ms) to 1s",
			elapsed)
	}
	if got := f.logs("DEBUG"); len(got) != 2 ||
		!strings.Contains(strings.Join(got, "\n"), "skip queryid=") {
		t.Fatalf("DEBUG lines = %q, want one skip per candidate", got)
	}
	if got := f.logs("INFO"); len(got) != 1 ||
		got[0] != "autoexplain: 2 candidates skipped: lock busy" {
		t.Fatalf("INFO lines = %q, want one summary", got)
	}
	if got := f.logs("WARN"); len(got) != 0 {
		t.Fatalf("a busy lock is not a warning, got %q", got)
	}
	if got := f.storedPlans(t, ids...); len(got) != 0 {
		t.Fatalf("lock-busy candidates must store nothing, got %v", got)
	}
}

// Error propagation: a statement timeout during planning is not a busy lock
// and keeps its WARN. The immutable function is folded at plan time.
func TestCollectStatementTimeoutStaysWarn(t *testing.T) {
	f := newLockFixture(t)
	const qid = int64(991200506)
	f.cleanPlans(t, qid)
	_, err := f.holder.Exec(context.Background(), `CREATE OR REPLACE FUNCTION
		explain_lock_slow_plan() RETURNS int IMMUTABLE LANGUAGE plpgsql AS
		$$ BEGIN PERFORM pg_sleep(6); RETURN 1; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.holder.Exec(context.Background(),
			"DROP FUNCTION IF EXISTS explain_lock_slow_plan()")
	})
	f.shadowCandidates(t, map[int64]string{
		qid: "SELECT * FROM " + lockTable + " WHERE id = explain_lock_slow_plan()",
	})

	if err := f.collector().Collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	warns := f.logs("WARN")
	if len(warns) != 1 || !strings.Contains(warns[0], "capture queryid=991200506") ||
		!strings.Contains(warns[0], "57014") {
		t.Fatalf("WARN lines = %q, want the statement timeout (57014)", warns)
	}
	if got := append(f.logs("DEBUG"), f.logs("INFO")...); len(got) != 0 {
		t.Fatalf("a statement timeout must not be reported as lock busy: %q", got)
	}
}
