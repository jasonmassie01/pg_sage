package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// vacuum-05: an executor VACUUM runs on a dedicated session with
// statement_timeout 0 under a watchdog that cancels it on a stall, at the
// cap, or on an emergency stop.

type vacuumFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	ctx   context.Context
	table string
	exec  *Executor

	mu       sync.Mutex
	settings map[string]string
	samples  []vacuumProgress
}

// newVacuumFixture creates a fresh table (every page unvisited, so a
// VACUUM scans them all, and autovacuum off) and an executor whose VACUUM session sleeps
// costDelay per page.
func newVacuumFixture(t *testing.T, rows int, costDelay string) *vacuumFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	return vacuumFixtureOn(t, pool, ctx, rows, costDelay)
}

// newFreezeFixture is newVacuumFixture on the exclusive server, for tests
// that assert relfrozenxid advanced: a freeze stops at the cluster's
// oldest xmin, which other packages' sessions on the shared server hold
// back (CI saw age 1531 after a completed VACUUM FREEZE).
func newFreezeFixture(t *testing.T, rows int, costDelay string) *vacuumFixture {
	t.Helper()
	pool, ctx := exclusiveDB(t, "relfrozenxid advances only past the cluster's oldest xmin")
	return vacuumFixtureOn(t, pool, ctx, rows, costDelay)
}

func vacuumFixtureOn(t *testing.T, pool *pgxpool.Pool, ctx context.Context, rows int,
	costDelay string) *vacuumFixture {
	t.Helper()
	f := &vacuumFixture{t: t, pool: pool, ctx: ctx,
		table: fmt.Sprintf("public.vac_%d", time.Now().UnixNano())}
	// Autovacuum off: an insert-triggered autovacuum would race the test's
	// own VACUUM (and the admission tests' "running vacuum").
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s WITH (autovacuum_enabled = false)
		AS SELECT g AS a, g %% 7 AS b FROM generate_series(1, %d) g`, f.table, rows)); err != nil {
		t.Fatalf("create %s: %v", f.table, err)
	}
	t.Cleanup(func() {
		terminateVacuumsOn(pool, f.table)
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+f.table)
	})
	cfg := config.DefaultConfig()
	cfg.Safety.LockTimeoutMs = 4321
	f.exec = New(pool, cfg, time.Time{}, nopLog)
	f.exec.emergencyStopFn = func(context.Context) bool { return false }
	f.exec.vacuumHooks = vacuumHooks{interval: 50 * time.Millisecond,
		onSession: f.throttle(costDelay)}
	return f
}

// throttle records the session's timeouts and slows its VACUUM.
func (f *vacuumFixture) throttle(costDelay string) func(context.Context, *pgxpool.Conn) error {
	return func(ctx context.Context, conn *pgxpool.Conn) error {
		got := map[string]string{}
		for _, name := range []string{"statement_timeout", "lock_timeout"} {
			var value string
			if err := conn.QueryRow(ctx, "SHOW "+name).Scan(&value); err != nil {
				return err
			}
			got[name] = value
		}
		f.mu.Lock()
		f.settings = got
		f.mu.Unlock()
		if _, err := conn.Exec(ctx, "SET vacuum_cost_delay = '"+costDelay+"'"); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, "SET vacuum_cost_limit = 1")
		return err
	}
}

// recordingReader samples the real progress view and keeps every sample.
func (f *vacuumFixture) recordingReader() vacuumProgressReader {
	return func(ctx context.Context, pid uint32) (vacuumProgress, bool, error) {
		p, ok, err := f.exec.readVacuumProgress(ctx, pid)
		if ok && err == nil {
			f.mu.Lock()
			f.samples = append(f.samples, p)
			f.mu.Unlock()
		}
		return p, ok, err
	}
}

func constantReader(context.Context, uint32) (vacuumProgress, bool, error) {
	return vacuumProgress{Phase: "scanning heap", HeapBlksTotal: 500, HeapBlksScanned: 3}, true,
		nil
}

func (f *vacuumFixture) vacuum() (time.Duration, error) {
	start := time.Now()
	sql := "VACUUM (FREEZE) " + f.table
	err := f.exec.execConcurrentDDL(f.ctx, sql, nil, WithLockTimeout(4321))
	return time.Since(start), err
}

func (f *vacuumFixture) vacuumRunning() bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*)
		FROM pg_stat_progress_vacuum WHERE relid = to_regclass($1)`, f.table).
		Scan(&n); err != nil {
		f.t.Fatalf("read progress: %v", err)
	}
	return n > 0
}

func terminateVacuumsOn(pool *pgxpool.Pool, table string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_progress_vacuum
		WHERE relid = to_regclass($1)`, table)
	for ctx.Err() == nil {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_progress_vacuum
			WHERE relid = to_regclass($1)`, table).Scan(&n); err != nil || n == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func requireCanceled(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("error %v, want the server's query_canceled (57014)", err)
	}
}

// CHECK-01 and the cap stand-in: ddl_timeout_seconds is 1 s, the VACUUM
// runs well past it with statement_timeout 0 while its progress advances,
// and completes.
func TestVacuumWatchedRunsPastOneSecondCapStandInAndCompletes(t *testing.T) {
	f := newFreezeFixture(t, 40000, "10ms")
	f.exec.cfg.Safety.DDLTimeoutSeconds = 1
	f.exec.vacuumHooks.readProgress = f.recordingReader()
	took, err := f.vacuum()
	if err != nil {
		t.Fatalf("watched VACUUM: %v", err)
	}
	if took < 1500*time.Millisecond {
		t.Fatalf("VACUUM took %s; the throttle did not hold it past the 1 s stand-in", took)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settings["statement_timeout"] != "0" {
		t.Fatalf("VACUUM session statement_timeout = %q, want 0", f.settings["statement_timeout"])
	}
	if f.settings["lock_timeout"] != "4321ms" {
		t.Fatalf("VACUUM session lock_timeout = %q, want it kept at 4321ms",
			f.settings["lock_timeout"])
	}
	if len(f.samples) < 2 {
		t.Fatalf("watchdog saw %d progress samples, want at least 2", len(f.samples))
	}
	first, last := f.samples[0], f.samples[len(f.samples)-1]
	if !progressAdvanced(first, last) {
		t.Fatalf("progress did not advance: first %+v last %+v", first, last)
	}
	var age int64
	if err := f.pool.QueryRow(f.ctx, `SELECT age(relfrozenxid)::bigint FROM pg_class
		WHERE oid = to_regclass($1)`, f.table).Scan(&age); err != nil {
		t.Fatalf("read age: %v", err)
	}
	if age > 1000 {
		t.Fatalf("relfrozenxid age after the completed freeze = %d, want it advanced", age)
	}
}

func progressAdvanced(a, b vacuumProgress) bool {
	return b.HeapBlksScanned > a.HeapBlksScanned || b.Phase != a.Phase ||
		b.HeapBlksVacuumed > a.HeapBlksVacuumed
}

func TestVacuumWatchedStallCancelsTheBackend(t *testing.T) {
	f := newVacuumFixture(t, 40000, "50ms")
	f.exec.vacuumHooks.readProgress = constantReader
	f.exec.vacuumHooks.stall = 300 * time.Millisecond
	took, err := f.vacuum()
	var watch *VacuumWatchError
	if !errors.As(err, &watch) || watch.Outcome != VacuumStalled {
		t.Fatalf("error %v, want a vacuum_stalled VacuumWatchError", err)
	}
	if watch.PID == 0 || watch.CancelErr != nil {
		t.Fatalf("watch = %+v, want the backend pid and a delivered cancel", watch)
	}
	requireCanceled(t, err)
	if took > 10*time.Second {
		t.Fatalf("stall cancel took %s", took)
	}
	if f.vacuumRunning() {
		t.Fatal("the stalled VACUUM is still running after the cancel")
	}
}

func TestVacuumWatchedRefusedCancelStillClosesTheConnection(t *testing.T) {
	f := newVacuumFixture(t, 40000, "50ms")
	f.exec.vacuumHooks.readProgress = constantReader
	f.exec.vacuumHooks.stall = 200 * time.Millisecond
	refused := errors.New("pg_cancel_backend refused: permission denied")
	f.exec.vacuumHooks.cancelBackend = func(context.Context, uint32) error { return refused }
	took, err := f.vacuum()
	var watch *VacuumWatchError
	if !errors.As(err, &watch) || watch.Outcome != VacuumStalled {
		t.Fatalf("error %v, want a vacuum_stalled VacuumWatchError", err)
	}
	if !errors.Is(err, refused) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error %v does not record the refused cancel", err)
	}
	if took > 10*time.Second {
		t.Fatalf("a refused cancel left the run waiting %s", took)
	}
}

func TestVacuumWatchedEmergencyStopCancelsWithinOneTick(t *testing.T) {
	f := newVacuumFixture(t, 40000, "50ms")
	var stopAt atomic.Int64
	var stopped atomic.Bool
	f.exec.vacuumHooks.emergencyStop = func(context.Context) (bool, error) {
		return stopped.Load(), nil
	}
	f.exec.vacuumHooks.readProgress = func(ctx context.Context, pid uint32) (
		vacuumProgress, bool, error) {
		if !stopped.Load() {
			stopAt.Store(time.Now().UnixNano())
			stopped.Store(true)
		}
		return f.exec.readVacuumProgress(ctx, pid)
	}
	_, err := f.vacuum()
	ended := time.Now()
	var watch *VacuumWatchError
	if !errors.As(err, &watch) || watch.Outcome != VacuumEmergencyStop {
		t.Fatalf("error %v, want an emergency_stop VacuumWatchError", err)
	}
	if !errors.Is(err, ErrEmergencyStop) {
		t.Fatalf("error %v does not match ErrEmergencyStop", err)
	}
	requireCanceled(t, err)
	// One 50 ms tick plus the cancel's round trips.
	if since := ended.Sub(time.Unix(0, stopAt.Load())); since > 3*time.Second {
		t.Fatalf("stop to cancel took %s, want within one watchdog tick", since)
	}
}

func TestVacuumWatchedCapCancels(t *testing.T) {
	f := newVacuumFixture(t, 40000, "50ms")
	f.exec.vacuumHooks.readProgress = f.recordingReader()
	f.exec.vacuumHooks.cap = 700 * time.Millisecond
	took, err := f.vacuum()
	var watch *VacuumWatchError
	if !errors.As(err, &watch) || watch.Outcome != VacuumCap {
		t.Fatalf("error %v, want a vacuum_cap VacuumWatchError", err)
	}
	requireCanceled(t, err)
	if took < 700*time.Millisecond || took > 10*time.Second {
		t.Fatalf("cap cancel after %s, want just after 700ms", took)
	}
}

// An emergency-stop flag that cannot be read does not cancel a VACUUM that
// may have run for hours (the in-process stop cancels it directly).
func TestVacuumWatchedUnreadableStopFlagDoesNotCancel(t *testing.T) {
	f := newVacuumFixture(t, 20000, "5ms")
	var reads atomic.Int32
	f.exec.vacuumHooks.emergencyStop = func(context.Context) (bool, error) {
		reads.Add(1)
		return true, errors.New("sage.config unreachable")
	}
	if _, err := f.vacuum(); err != nil {
		t.Fatalf("VACUUM with an unreadable stop flag: %v", err)
	}
	if reads.Load() == 0 {
		t.Fatal("the watchdog never read the stop flag")
	}
}

// The in-process emergency stop (CancelInFlight) cancels the watched
// VACUUM's statement directly and reports a StoppedError.
func TestVacuumWatchedInFlightStopCancelsTheStatement(t *testing.T) {
	f := newVacuumFixture(t, 40000, "50ms")
	f.exec.vacuumHooks.readProgress = f.recordingReader()
	ctx, run, err := f.exec.inflight.begin(f.ctx, "VACUUM")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	defer f.exec.inflight.end(run, 0)
	go func() {
		for !f.vacuumRunning() {
			time.Sleep(20 * time.Millisecond)
		}
		_ = run.stop(context.Background(), ErrEmergencyStop)
	}()
	err = f.exec.execConcurrentDDL(ctx, "VACUUM (FREEZE) "+f.table, nil, WithLockTimeout(4321))
	var stopped *StoppedError
	if !errors.As(err, &stopped) || !errors.Is(err, ErrEmergencyStop) {
		t.Fatalf("error %v, want a StoppedError of the emergency stop", err)
	}
	if f.vacuumRunning() {
		t.Fatal("the VACUUM still runs after the in-process stop")
	}
}

// A progress probe that fails never cancels: the VACUUM completes.
func TestVacuumWatchedProbeErrorsDoNotCancel(t *testing.T) {
	f := newVacuumFixture(t, 20000, "5ms")
	probeErr := errors.New("probe timed out")
	var probes atomic.Int32
	f.exec.vacuumHooks.readProgress = func(context.Context, uint32) (
		vacuumProgress, bool, error) {
		probes.Add(1)
		return vacuumProgress{}, false, probeErr
	}
	f.exec.vacuumHooks.stall = 100 * time.Millisecond
	if _, err := f.vacuum(); err != nil {
		t.Fatalf("VACUUM with failing probes: %v", err)
	}
	if probes.Load() == 0 {
		t.Fatal("the watchdog never probed")
	}
}

// A probe that hangs (a pool with no free connection, a stalled network)
// is bounded, so the watchdog still cancels at the cap; unbounded, it held
// the watchdog until the session itself was closed cap+30 s later.
func TestVacuumWatchedHungProbeStillCancelsAtTheCap(t *testing.T) {
	f := newVacuumFixture(t, 80000, "50ms")
	f.exec.vacuumHooks.readProgress = func(ctx context.Context, _ uint32) (
		vacuumProgress, bool, error) {
		<-ctx.Done()
		return vacuumProgress{}, false, ctx.Err()
	}
	f.exec.vacuumHooks.cap = 200 * time.Millisecond
	took, err := f.vacuum()
	var watch *VacuumWatchError
	if !errors.As(err, &watch) || watch.Outcome != VacuumCap {
		t.Fatalf("error %v after %s, want a vacuum_cap VacuumWatchError", err, took)
	}
	requireCanceled(t, err)
	if took > vacuumCancelTimeout+5*time.Second {
		t.Fatalf("cap cancel after %s, want within one probe bound of the cap", took)
	}
}

// A missing progress row (the VACUUM has not reached the heap yet, or has
// finished) is not a stall either.
func TestVacuumWatchedMissingProgressRowIsNotAStall(t *testing.T) {
	f := newVacuumFixture(t, 20000, "5ms")
	f.exec.vacuumHooks.readProgress = func(context.Context, uint32) (
		vacuumProgress, bool, error) {
		return vacuumProgress{}, false, nil
	}
	f.exec.vacuumHooks.stall = 100 * time.Millisecond
	if _, err := f.vacuum(); err != nil {
		t.Fatalf("VACUUM without a progress row: %v", err)
	}
}

// The real reader reads the backend's own row; another pid has none.
func TestReadVacuumProgressOfAnIdleBackendIsAbsent(t *testing.T) {
	f := newVacuumFixture(t, 10, "0")
	var pid uint32
	if err := f.pool.QueryRow(f.ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	p, ok, err := f.exec.readVacuumProgress(f.ctx, pid)
	if err != nil || ok {
		t.Fatalf("idle backend progress = %+v ok=%v err=%v, want absent", p, ok, err)
	}
}

// VACUUM FULL never reaches the watched path: it keeps the transaction
// refusal and ddl timeout of every blocking statement.
func TestVacuumFullIsNotWatched(t *testing.T) {
	if isWatchedVacuum("VACUUM FULL public.t") || isWatchedVacuum("VACUUM (FULL) public.t") {
		t.Fatal("VACUUM FULL routed to the watched VACUUM")
	}
	if !isWatchedVacuum("VACUUM (FREEZE) public.t") || !isWatchedVacuum("vacuum public.t") {
		t.Fatal("a plain VACUUM is not watched")
	}
	if isWatchedVacuum("ANALYZE public.t") || isWatchedVacuum("ALTER SYSTEM SET x = 1") {
		t.Fatal("a non-VACUUM statement routed to the watched VACUUM")
	}
}
