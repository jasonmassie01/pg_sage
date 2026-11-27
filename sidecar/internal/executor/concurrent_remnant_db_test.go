package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// executor-05: a failed CONCURRENTLY statement's INVALID remnant is
// dropped only when it is provably the failed statement's own object.

type remnantFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	ctx   context.Context
	bare  string
	table string
	exec  *Executor
}

func newRemnantFixture(t *testing.T, lockTimeoutMS int) *remnantFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	bare := fmt.Sprintf("rmnt_%d", time.Now().UnixNano())
	f := &remnantFixture{t: t, pool: pool, ctx: ctx, bare: bare, table: "public." + bare}
	f.must("CREATE TABLE " + f.table + " (a int NOT NULL, b int NOT NULL)")
	f.must("INSERT INTO " + f.table + " SELECT g, g % 7 FROM generate_series(1, 500) g")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+f.table) })
	cfg := config.DefaultConfig()
	cfg.Safety.LockTimeoutMs = lockTimeoutMS
	f.exec = New(pool, cfg, time.Time{}, nopLog)
	f.exec.emergencyStopFn = func(context.Context) bool { return false }
	return f
}

func (f *remnantFixture) must(sql string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// invalidRemnant leaves an INVALID index named name (its build divides by
// zero) and returns its OID.
func (f *remnantFixture) invalidRemnant(name string) int64 {
	f.t.Helper()
	_, _ = f.pool.Exec(f.ctx, "CREATE INDEX CONCURRENTLY "+name+" ON "+f.table+" ((b / 0))")
	exists, valid, oid := f.index("public." + name)
	if !exists || valid {
		f.t.Fatalf("setup: %s exists=%v valid=%v", name, exists, valid)
	}
	return oid
}

func (f *remnantFixture) index(qualified string) (exists, valid bool, oid int64) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(i.indexrelid::bigint, 0),
		COALESCE(i.indisvalid, false) FROM (SELECT to_regclass($1) AS oid) r
		LEFT JOIN pg_index i ON i.indexrelid = r.oid`, qualified).Scan(&oid, &valid)
	if err != nil {
		f.t.Fatalf("read index %s: %v", qualified, err)
	}
	return oid > 0, valid, oid
}

func (f *remnantFixture) tableOID() int64 {
	f.t.Helper()
	var oid int64
	if err := f.pool.QueryRow(f.ctx, "SELECT to_regclass($1)::oid::bigint", f.table).
		Scan(&oid); err != nil {
		f.t.Fatalf("table oid: %v", err)
	}
	return oid
}

// invalidCount is what the analyzer's invalid_index rule would see.
func invalidCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_index
		WHERE indrelid = to_regclass($1) AND NOT indisvalid`, table).Scan(&n); err != nil {
		t.Fatalf("count invalid indexes: %v", err)
	}
	return n
}

// holdSnapshot opens a REPEATABLE READ transaction on another table: it
// holds no lock on the target, but every CONCURRENTLY statement's last
// wait (for older snapshots) waits for it until lock_timeout.
func holdSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) func() {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin snapshot holder: %v", err)
	}
	release := func() { _ = tx.Rollback(context.Background()) }
	t.Cleanup(release)
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		t.Fatalf("snapshot holder isolation: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT count(*) FROM pg_class"); err != nil {
		t.Fatalf("snapshot holder: %v", err)
	}
	return release
}

// holdLock holds mode on the table until released.
func holdLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, mode string) func() {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	release := func() { _ = tx.Rollback(context.Background()) }
	t.Cleanup(release)
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN "+mode+" MODE"); err != nil {
		t.Fatalf("lock %s: %v", table, err)
	}
	return release
}

func cleanupOf(t *testing.T, state map[string]any) *remnantCleanup {
	t.Helper()
	c, ok := state[remnantCleanupKey].(*remnantCleanup)
	if !ok {
		t.Fatalf("no remnant_cleanup recorded: %v", state)
	}
	return c
}

// A build whose last wait is cancelled by lock_timeout after its catalog
// entry exists: the remnant is dropped, bound to the OID it registered.
func TestFailedBuildDropsItsOwnRemnant(t *testing.T) {
	f := newRemnantFixture(t, 500)
	release := holdSnapshot(t, f.ctx, f.pool)
	state := map[string]any{}
	sql := "CREATE INDEX CONCURRENTLY " + f.bare + "_a ON " + f.table + " (a)"
	err := f.exec.execConcurrentDDL(f.ctx, sql, state, WithLockTimeout(500))
	release()
	if !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("build error = %v, want the lock timeout", err)
	}
	if n := invalidCount(t, f.ctx, f.pool, f.table); n != 0 {
		t.Fatalf("CHECK-01: %d INVALID indexes remain on %s", n, f.table)
	}
	c := cleanupOf(t, state)
	if c.Outcome != remnantDropped || len(c.Indexes) != 1 || c.Indexes[0].OID <= 0 ||
		c.Indexes[0].Outcome != remnantDropped {
		t.Fatalf("cleanup = %+v, want one dropped index with its OID", c)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("OID %d", c.Indexes[0].OID)) {
		t.Fatalf("error %q does not name the dropped OID", err)
	}
	if err := f.exec.execConcurrentDDL(f.ctx, sql, nil, WithLockTimeout(500)); err != nil {
		t.Fatalf("the retry after the cleanup: %v", err)
	}
	if exists, valid, _ := f.index("public." + f.bare + "_a"); !exists || !valid {
		t.Fatalf("the retry built exists=%v valid=%v, want a valid index", exists, valid)
	}
}

// A build that fails a moment after registering its index (division by
// zero) is still bound: the gate holds it until its OID is read.
func TestFastFailingBuildIsStillBound(t *testing.T) {
	f := newRemnantFixture(t, 2000)
	for i := range 5 {
		state := map[string]any{}
		sql := fmt.Sprintf("CREATE INDEX CONCURRENTLY %s_z%d ON %s ((b / 0))", f.bare, i, f.table)
		err := f.exec.execConcurrentDDL(f.ctx, sql, state, WithLockTimeout(2000))
		if err == nil || !strings.Contains(err.Error(), "division by zero") {
			t.Fatalf("attempt %d: error = %v, want division by zero", i, err)
		}
		if c := cleanupOf(t, state); c.Outcome != remnantDropped {
			t.Fatalf("attempt %d: cleanup = %+v, want dropped", i, c)
		}
	}
	if n := invalidCount(t, f.ctx, f.pool, f.table); n != 0 {
		t.Fatalf("%d INVALID indexes remain after five fast failures", n)
	}
}

// A failed REINDEX CONCURRENTLY drops the copy it added, keeps the
// original valid and never touches an older x_ccnew of someone else's.
func TestFailedReindexDropsOnlyItsOwnCopy(t *testing.T) {
	f := newRemnantFixture(t, 500)
	f.must("CREATE INDEX " + f.bare + "_a ON " + f.table + " (a)")
	foreign := f.invalidRemnant(f.bare + "_a_ccnew")
	release := holdSnapshot(t, f.ctx, f.pool)
	state := map[string]any{}
	err := f.exec.execConcurrentDDL(f.ctx, "REINDEX INDEX CONCURRENTLY public."+f.bare+"_a",
		state, WithLockTimeout(500))
	release()
	if !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("reindex error = %v, want the lock timeout", err)
	}
	if exists, valid, _ := f.index("public." + f.bare + "_a"); !exists || !valid {
		t.Fatalf("the original exists=%v valid=%v, want it valid", exists, valid)
	}
	if exists, _, oid := f.index("public." + f.bare + "_a_ccnew"); !exists || oid != foreign {
		t.Fatalf("the pre-existing x_ccnew was touched: exists=%v oid=%d", exists, oid)
	}
	if n := invalidCount(t, f.ctx, f.pool, f.table); n != 1 {
		t.Fatalf("CHECK-02: %d INVALID indexes, want only the pre-existing x_ccnew", n)
	}
	c := cleanupOf(t, state)
	if c.Outcome != remnantDropped || len(c.Indexes) != 1 ||
		!strings.Contains(c.Indexes[0].Name, "_ccnew1") {
		t.Fatalf("cleanup = %+v, want the dropped _ccnew1 copy", c)
	}
}

// A VALID index under the name is kept; an index re-created by hand under
// the name (another OID) is kept and recorded as oid_mismatch.
func TestRemnantIdentityKeepsOtherObjects(t *testing.T) {
	f := newRemnantFixture(t, 1000)
	f.must("CREATE INDEX " + f.bare + "_v ON " + f.table + " (a)")
	_, _, validOID := f.index("public." + f.bare + "_v")
	stale := f.invalidRemnant(f.bare + "_r")
	f.must("DROP INDEX public." + f.bare + "_r")
	f.invalidRemnant(f.bare + "_r") // re-created by hand: a new OID
	run := f.exec.remnantRunFor("DROP INDEX CONCURRENTLY x", WithLockTimeout(1000))
	c := run.dropRemnants(f.ctx, []remnantCandidate{
		{Index: "public." + f.bare + "_v", OID: validOID, TableOID: f.tableOID()},
		{Index: "public." + f.bare + "_r", OID: stale, TableOID: f.tableOID()},
	})
	if c.Indexes[0].Outcome != remnantValidKept || c.Indexes[1].Outcome != remnantOIDMismatch {
		t.Fatalf("outcomes = %+v, want valid_kept and oid_mismatch", c.Indexes)
	}
	if c.Outcome != remnantOIDMismatch {
		t.Fatalf("overall = %s, want oid_mismatch", c.Outcome)
	}
	for _, name := range []string{"_v", "_r"} {
		if exists, _, _ := f.index("public." + f.bare + name); !exists {
			t.Fatalf("%s was dropped", name)
		}
	}
}

// A build stopped in phase 1 (an ACCESS EXCLUSIVE holder) registered
// nothing: nothing is dropped and the cleanup raises no error.
func TestPhaseOneFailureHasNothingToDrop(t *testing.T) {
	f := newRemnantFixture(t, 300)
	release := holdLock(t, f.ctx, f.pool, f.table, "ACCESS EXCLUSIVE")
	state := map[string]any{}
	sql := "CREATE INDEX CONCURRENTLY " + f.bare + "_a ON " + f.table + " (a)"
	err := f.exec.execConcurrentDDL(f.ctx, sql, state, WithLockTimeout(300))
	release()
	if !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("build error = %v, want the lock timeout", err)
	}
	c := cleanupOf(t, state)
	if c.Outcome != remnantNothingToDrop || len(c.Indexes) != 0 {
		t.Fatalf("cleanup = %+v, want nothing_to_drop", c)
	}
	if strings.Contains(err.Error(), remnantCleanupFailed) {
		t.Fatalf("error %q reports a cleanup failure", err)
	}
	if exists, _, _ := f.index("public." + f.bare + "_a"); exists {
		t.Fatal("an index exists after a phase-1 failure")
	}
}

// A writer that outlives the failed build also blocks its cleanup DROP:
// the lock timeout is recorded as remnant_cleanup_failed and the remnant
// is left to the analyzer.
func TestCleanupDropLockTimeoutIsRecorded(t *testing.T) {
	f := newRemnantFixture(t, 400)
	release := holdLock(t, f.ctx, f.pool, f.table, "ROW EXCLUSIVE")
	state := map[string]any{}
	sql := "CREATE INDEX CONCURRENTLY " + f.bare + "_a ON " + f.table + " (a)"
	err := f.exec.execConcurrentDDL(f.ctx, sql, state, WithLockTimeout(400))
	release()
	if !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("build error = %v, want the lock timeout", err)
	}
	c := cleanupOf(t, state)
	if c.Outcome != remnantCleanupFailed || len(c.Indexes) != 1 ||
		!strings.Contains(c.Indexes[0].Error, "lock") {
		t.Fatalf("cleanup = %+v, want remnant_cleanup_failed naming the lock timeout", c)
	}
	if exists, valid, oid := f.index("public." + f.bare + "_a"); !exists || valid ||
		oid != c.Indexes[0].OID {
		t.Fatalf("remnant exists=%v valid=%v oid=%d, want the INVALID remnant left", exists,
			valid, oid)
	}
	if !strings.Contains(err.Error(), remnantCleanupFailed) {
		t.Fatalf("error %q does not report the failed cleanup", err)
	}
}

// Two executors cleaning the same remnant: one drop, the other a no-op
// without a failure.
func TestConcurrentCleanupDropsOnce(t *testing.T) {
	f := newRemnantFixture(t, 2000)
	oid := f.invalidRemnant(f.bare + "_r")
	cand := remnantCandidate{Index: "public." + f.bare + "_r", OID: oid, TableOID: f.tableOID()}
	var barrier sync.WaitGroup
	barrier.Add(2)
	results := make([]*remnantCleanup, 2)
	var done sync.WaitGroup
	for i := range 2 {
		done.Add(1)
		go func() {
			defer done.Done()
			run := f.exec.remnantRunFor("DROP INDEX CONCURRENTLY x", WithLockTimeout(2000))
			run.beforeDrop = func(context.Context) error {
				barrier.Done()
				barrier.Wait()
				return nil
			}
			results[i] = run.dropRemnants(f.ctx, []remnantCandidate{cand})
		}()
	}
	done.Wait()
	outcomes := map[string]int{}
	for _, r := range results {
		outcomes[r.Indexes[0].Outcome]++
	}
	if outcomes[remnantDropped] != 1 || outcomes[remnantCleanupFailed] != 0 ||
		outcomes[remnantInProgress]+outcomes[remnantGone] != 1 {
		t.Fatalf("outcomes = %v, want one drop and one no-op", outcomes)
	}
	if exists, _, _ := f.index(cand.Index); exists {
		t.Fatal("the remnant survived both cleanups")
	}
}

// An operator-approved CREATE INDEX over an INVALID index of its name is
// refused with the command to clear it; the index is never dropped by name.
func TestManualCreateRefusesAnUnboundInvalidIndex(t *testing.T) {
	f := newRemnantFixture(t, 1000)
	sql := "CREATE INDEX CONCURRENTLY " + f.bare + "_r ON " + f.table + " (a)"
	if err := f.exec.refuseUnboundRemnant(f.ctx, sql); err != nil {
		t.Fatalf("no index of the name: %v", err)
	}
	oid := f.invalidRemnant(f.bare + "_r")
	err := f.exec.refuseUnboundRemnant(f.ctx, sql)
	if err == nil || !strings.Contains(err.Error(), "INVALID") ||
		!strings.Contains(err.Error(), "DROP INDEX CONCURRENTLY IF EXISTS") {
		t.Fatalf("refusal = %v, want an actionable INVALID-index error", err)
	}
	if _, _, got := f.index("public." + f.bare + "_r"); got != oid {
		t.Fatalf("the INVALID index was touched: oid %d, want %d", got, oid)
	}
}

// The replacement's failed build records its cleanup in the action row.
func TestReplaceBuildFailureRecordsItsCleanup(t *testing.T) {
	f := newReplaceFixture(t, "(b / 0)")
	id, err := f.run()
	if !errors.Is(err, ErrReplaceCreateFailed) || id <= 0 {
		t.Fatalf("replace = %d, %v; want a recorded create failure", id, err)
	}
	var raw []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT before_state->'remnant_cleanup'
		FROM sage.action_log WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read the action row: %v", err)
	}
	var c remnantCleanup
	if err := json.Unmarshal(raw, &c); err != nil || c.Outcome != remnantDropped ||
		len(c.Indexes) != 1 || c.Indexes[0].OID <= 0 {
		t.Fatalf("remnant_cleanup = %s (%v), want one dropped index", raw, err)
	}
}

// A resumed build whose OID was never recorded leaves the remnant to the
// invalid_index finding and says so.
func TestResumeWithoutRecordedOIDKeepsTheRemnant(t *testing.T) {
	f := newReplaceFixture(t, "(b / 0)")
	if _, err := f.run(); !errors.Is(err, ErrReplaceCreateFailed) {
		t.Fatalf("setup: %v", err)
	}
	_, _ = f.pool.Exec(f.ctx, "CREATE INDEX CONCURRENTLY "+f.bare+"_a_b ON "+f.table+
		" (a, (b / 0))")
	f.setState(replaceCreating)
	f.must(t, "UPDATE sage.index_replace SET new_index_oid = NULL WHERE table_name = '"+
		f.table+"'")
	if err := f.restarted().ResumeIndexReplaces(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if exists, valid := f.index(f.newIndex); !exists || valid {
		t.Fatalf("unbound remnant exists=%v valid=%v, want it kept", exists, valid)
	}
	if r := f.row(); r.state != replaceCreateFailed ||
		!strings.Contains(r.err, "left to the invalid_index finding") {
		t.Fatalf("state row = %+v, want create_failed naming why the remnant stays", r)
	}
}
