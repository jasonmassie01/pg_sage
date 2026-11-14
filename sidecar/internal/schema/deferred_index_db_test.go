package schema

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// config-04c on a real server: large sage indexes are left out of the
// boot and built CONCURRENTLY in the background, one builder per database.

const deferredTestIndex = "idx_decision_created"

// startupDeadline is the sidecar's metaDBTimeout: a boot must fit in it.
const startupDeadline = 10 * time.Second

// setDeferredRows makes every deferred index's table look rows big.
func setDeferredRows(t *testing.T, rows float64) {
	t.Helper()
	saved := deferredRowsFn
	deferredRowsFn = func(_ context.Context, _ bootstrapDB, tables []string) (
		map[string]float64, error) {
		out := make(map[string]float64, len(tables))
		for _, table := range tables {
			out[table] = rows
		}
		return out, nil
	}
	t.Cleanup(func() { deferredRowsFn = saved })
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// forgetDeferredStep drops the test index and the step's ledger row: the
// state of an install whose step has not been recorded yet.
func forgetDeferredStep(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, "DROP INDEX IF EXISTS sage."+deferredTestIndex)
	mustExec(t, pool, "DELETE FROM sage.schema_migrations WHERE id = 'ddlDecisionLedger'")
}

func bootWithin(t *testing.T, pool *pgxpool.Pool, deadline time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot (after %s): %v", time.Since(start), err)
	}
}

// Boundary: 999,999 rows and never analyzed (-1) build inline and record
// the step; 1,000,000 runs the variant and leaves the step unrecorded.
func TestBootstrap_DeferralBoundary(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_boundary")
	cases := []struct {
		rows     float64
		deferred bool
	}{{999_999, false}, {1_000_000, true}, {-1, false}, {0, false}}
	for _, c := range cases {
		forgetDeferredStep(t, pool)
		setDeferredRows(t, c.rows)
		bootWithin(t, pool, startupDeadline)
		exists, valid := indexValid(t, pool, deferredTestIndex)
		_, recorded := ledgerRows(t, pool)["ddlDecisionLedger"]
		if c.deferred && (exists || recorded) {
			t.Errorf("rows %v: index exists %v, step recorded %v; want both false",
				c.rows, exists, recorded)
		}
		if !c.deferred && (!exists || !valid || !recorded) {
			t.Errorf("rows %v: index exists %v valid %v, step recorded %v; want all true",
				c.rows, exists, valid, recorded)
		}
		if recorded && ledgerRows(t, pool)["ddlDecisionLedger"] !=
			stepChecksum(ddlDecisionLedger()) {
			t.Errorf("rows %v: step recorded with a checksum other than the released one",
				c.rows)
		}
	}
}

func seedDecisions(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier,
		reason, evidence_id) SELECT 'f', 'i', 'parked', 'safe', 'r', 'e' || g
		FROM generate_series(1, $1) g`, n)
	mustExec(t, pool, "ANALYZE sage.decision")
}

// Happy path: a real table above the threshold boots within the startup
// deadline without the index; the builder then creates it VALID and the
// gauge goes to zero; the next boot records the released step.
func TestDeferredIndexes_HappyPath(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_happy")
	forgetDeferredStep(t, pool)
	seedDecisions(t, pool, deferredIndexRows)
	bootWithin(t, pool, startupDeadline)
	if exists, _ := indexValid(t, pool, deferredTestIndex); exists {
		t.Fatal("the boot built the deferred index inline")
	}
	if _, ok := ledgerRows(t, pool)["ddlDecisionLedger"]; ok {
		t.Fatal("the boot recorded a step it ran without its deferred indexes")
	}
	var observed []DeferredIndexCounts
	b := &DeferredIndexBuilder{Pool: pool, Database: "happy",
		Observe: func(c DeferredIndexCounts) { observed = append(observed, c) }}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b.Run(ctx)
	if exists, valid := indexValid(t, pool, deferredTestIndex); !exists || !valid {
		t.Fatalf("after the builder: exists %v valid %v, want a VALID index", exists, valid)
	}
	if len(observed) < 2 || observed[0].Missing != 1 {
		t.Fatalf("observed %v, want missing=1 first", observed)
	}
	if last := observed[len(observed)-1]; last != (DeferredIndexCounts{}) {
		t.Fatalf("last gauge sample %v, want zero", last)
	}
	bootWithin(t, pool, startupDeadline)
	if got := ledgerRows(t, pool)["ddlDecisionLedger"]; got != stepChecksum(ddlDecisionLedger()) {
		t.Fatalf("ddlDecisionLedger recorded %q, want the released checksum", got)
	}
}

// Nil/empty: a fresh schema builds every index inline; the builder finds
// nothing to do.
func TestBuildDeferredIndexes_FreshSchemaHasNothingToDo(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_fresh")
	report, err := BuildDeferredIndexes(context.Background(), pool)
	if err != nil {
		t.Fatalf("build on a fresh schema: %v", err)
	}
	if len(report.Built)+len(report.Failed)+len(report.Building)+len(report.Skipped) != 0 {
		t.Fatalf("fresh schema report %+v, want nothing done", report)
	}
	counts, err := ReadDeferredIndexStates(context.Background(), pool)
	if err != nil || counts != (DeferredIndexCounts{}) {
		t.Fatalf("fresh schema states %+v, %v; want zero", counts, err)
	}
}

// Invalid input: a spec whose table is missing is skipped, not an error.
func TestBuildDeferredIndexes_MissingTableIsSkipped(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_missing_table")
	saved := deferredIndexes
	deferredIndexes = []ledgerIndex{{"idx_nope", "no_such_table",
		"INDEX %I ON sage.no_such_table (x)"}}
	t.Cleanup(func() { deferredIndexes = saved })
	report, err := BuildDeferredIndexes(context.Background(), pool)
	if err != nil {
		t.Fatalf("missing table: %v", err)
	}
	if len(report.Skipped) != 1 || report.Skipped[0] != "idx_nope" || len(report.Built) != 0 {
		t.Fatalf("report %+v, want idx_nope skipped and nothing built", report)
	}
	counts, err := ReadDeferredIndexStates(context.Background(), pool)
	if err != nil || counts != (DeferredIndexCounts{}) {
		t.Fatalf("states %+v, %v; a missing table is not a missing index", counts, err)
	}
}

// holdLock opens a transaction on its own connection holding lockSQL; the
// returned func rolls it back.
func holdLock(t *testing.T, pool *pgxpool.Pool, lockSQL string) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := tx.Exec(ctx, lockSQL); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("%s: %v", lockSQL, err)
	}
	done := false
	release := func() {
		if !done {
			done = true
			_ = tx.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	return release
}

// Error propagation: lock_timeout on the CIC returns an error naming the
// index and its table; the other indexes are untouched.
func TestBuildDeferredIndexes_LockTimeoutNamesIndexAndTable(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_lock_timeout")
	mustExec(t, pool, "DROP INDEX sage."+deferredTestIndex)
	release := holdLock(t, pool, "LOCK TABLE sage.decision IN SHARE ROW EXCLUSIVE MODE")
	start := time.Now()
	report, err := BuildDeferredIndexes(context.Background(), pool)
	took := time.Since(start)
	release()
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != sqlLockNotAvailable {
		t.Fatalf("build behind a lock: err %v, want lock_timeout (55P03)", err)
	}
	if !strings.Contains(err.Error(), deferredTestIndex) ||
		!strings.Contains(err.Error(), "sage.decision") {
		t.Fatalf("error %q does not name the index and its table", err)
	}
	if took > 10*time.Second {
		t.Fatalf("build waited %s for the lock, want about lock_timeout (2s)", took)
	}
	if len(report.Failed) != 1 || report.Failed[0] != deferredTestIndex {
		t.Fatalf("report %+v, want %s failed", report, deferredTestIndex)
	}
	if exists, valid := indexValid(t, pool, "idx_decision_fingerprint"); !exists || !valid {
		t.Fatal("a failed build touched another index")
	}
	if _, err := BuildDeferredIndexes(context.Background(), pool); err != nil {
		t.Fatalf("retry after the lock is gone: %v", err)
	}
	if exists, valid := indexValid(t, pool, deferredTestIndex); !exists || !valid {
		t.Fatalf("after the retry: exists %v valid %v", exists, valid)
	}
}

// Concurrency: a builder holding the lock makes the other report "another
// sidecar is building".
func TestBuildDeferredIndexes_SecondBuilderIsBusy(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_busy")
	mustExec(t, pool, "DROP INDEX sage."+deferredTestIndex)
	release := holdLock(t, pool, "LOCK TABLE sage.decision IN SHARE ROW EXCLUSIVE MODE")
	type result struct {
		report BuildReport
		err    error
	}
	first := make(chan result, 1)
	go func() {
		r, err := BuildDeferredIndexes(context.Background(), pool)
		first <- result{r, err}
	}()
	waitFor(t, pool, `SELECT count(*) > 0 FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'
		  AND query ILIKE 'CREATE INDEX CONCURRENTLY%'`)
	_, err := BuildDeferredIndexes(context.Background(), pool)
	if !errors.Is(err, ErrDeferredBuildBusy) ||
		!strings.Contains(err.Error(), "another sidecar is building") {
		t.Fatalf("second builder: err %v, want ErrDeferredBuildBusy", err)
	}
	release()
	r := <-first
	if r.err != nil || len(r.report.Built) != 1 {
		t.Fatalf("first builder: %+v, %v; want %s built", r.report, r.err, deferredTestIndex)
	}
	if exists, valid := indexValid(t, pool, deferredTestIndex); !exists || !valid {
		t.Fatalf("after the first builder: exists %v valid %v", exists, valid)
	}
}

// waitFor polls query (one boolean) until it is true, for at most 20 s.
func waitFor(t *testing.T, pool *pgxpool.Pool, query string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var ok bool
		if err := pool.QueryRow(context.Background(), query).Scan(&ok); err != nil {
			t.Fatalf("poll %s: %v", query, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", query)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const noBuildRunning = `SELECT count(*) = 0 FROM pg_stat_activity
	WHERE datname = current_database() AND query ILIKE 'CREATE INDEX CONCURRENTLY%'
	  AND state <> 'idle'`

// Concurrency (the deadlock in decision_ledger_migration.go): a boot while
// a build holds its SHARE UPDATE EXCLUSIVE lock on sage.decision, over an
// untrusted ledger, completes within the startup deadline.
func TestBootstrap_DuringABuildCompletes(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_reload")
	if _, err := pool.Exec(context.Background(), `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = to_regclass('sage.' || $1)`, deferredTestIndex); err != nil {
		t.Skipf("cannot mark an index invalid (needs superuser): %v", err)
	}
	mustExec(t, pool, "DELETE FROM sage.schema_migrations WHERE id = $1", ledgerCatalogID)
	setDeferredRows(t, 2_000_000)
	release := holdLock(t, pool, "LOCK TABLE sage.decision IN SHARE UPDATE EXCLUSIVE MODE")
	bootWithin(t, pool, startupDeadline)
	release()
	if exists, valid := indexValid(t, pool, deferredTestIndex); !exists || valid {
		t.Fatalf("the boot touched the index being built: exists %v valid %v", exists, valid)
	}
	report, err := BuildDeferredIndexes(context.Background(), pool)
	if err != nil || len(report.Built) != 1 {
		t.Fatalf("builder after the boot: %+v, %v", report, err)
	}
	if exists, valid := indexValid(t, pool, deferredTestIndex); !exists || !valid {
		t.Fatalf("after the builder: exists %v valid %v", exists, valid)
	}
}

// openWriter keeps a transaction that wrote sage.decision open: a CIC
// waits for it after creating its INVALID index.
func openWriter(t *testing.T, pool *pgxpool.Pool) func() {
	t.Helper()
	return holdLock(t, pool, `INSERT INTO sage.decision (feature, intent, verdict,
		risk_tier, reason, evidence_id) VALUES ('f', 'i', 'parked', 'safe', 'r', 'w')`)
}

const cicWaitingOnWriter = `SELECT to_regclass('sage.idx_decision_created') IS NOT NULL
	AND EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
	              AND query ILIKE 'CREATE INDEX CONCURRENTLY%' AND wait_event_type = 'Lock')`

// interruptBuild starts the builder, waits until its CIC created the
// INVALID index and waits on the open writer, then cancels it.
func interruptBuild(t *testing.T, pool *pgxpool.Pool, run func(context.Context) error) error {
	t.Helper()
	release := openWriter(t, pool)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	waitFor(t, pool, cicWaitingOnWriter)
	cancel()
	select {
	case err := <-done:
		waitFor(t, pool, noBuildRunning)
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the builder did not return after its context was cancelled")
		return nil
	}
}

func ledgerTrusted(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	l := &migrationLedger{}
	if err := l.load(context.Background(), conn); err != nil {
		t.Fatalf("load the ledger: %v", err)
	}
	return l.trusted
}

// State transitions: a cancelled build leaves an INVALID index that does
// not distrust the ledger; the next boot leaves it alone ("N recorded");
// the next build replaces it VALID; the boot after that issues no CREATE
// or DROP INDEX and records the released step (CHECK-20).
func TestDeferredIndexes_InterruptedBuildRecovers(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_interrupt")
	forgetDeferredStep(t, pool)
	setDeferredRows(t, 2_000_000)
	bootWithin(t, pool, startupDeadline)
	err := interruptBuild(t, pool, func(ctx context.Context) error {
		_, err := BuildDeferredIndexes(ctx, pool)
		return err
	})
	if err == nil {
		t.Fatal("a cancelled build returned no error")
	}
	if exists, valid := indexValid(t, pool, deferredTestIndex); !exists || valid {
		t.Fatalf("after the cancel: exists %v valid %v, want an INVALID index", exists, valid)
	}
	if !ledgerTrusted(t, pool) {
		t.Fatal("the deferred INVALID index distrusts the ledger")
	}
	logs := captureLog(t)
	bootWithin(t, pool, startupDeadline)
	logged := logs.String()
	if !strings.Contains(logged, " recorded") {
		t.Fatalf("restart after the interrupted build logged no recorded steps:\n%s", logged)
	}
	if strings.Contains(logged, "INVALID") {
		t.Fatalf("restart warned about the deferred index:\n%s", logged)
	}
	if exists, valid := indexValid(t, pool, deferredTestIndex); !exists || valid {
		t.Fatalf("the boot rebuilt a large-table index inline: exists %v valid %v",
			exists, valid)
	}
	report, err := BuildDeferredIndexes(context.Background(), pool)
	if err != nil || len(report.Built) != 1 || report.Built[0] != deferredTestIndex {
		t.Fatalf("rebuild: %+v, %v", report, err)
	}
	assertNoIndexDDL(t, pool)
	if got := ledgerRows(t, pool)["ddlDecisionLedger"]; got != stepChecksum(ddlDecisionLedger()) {
		t.Fatalf("ddlDecisionLedger recorded %q, want the released checksum", got)
	}
	assertBootRunsNoDDL(t, pool)
}

// assertNoIndexDDL boots once more and fails on any top-level CREATE or
// DROP INDEX, or a deferred index the boot's DO block replaced (its oid
// changed). The step's DO block itself runs once, to be recorded.
func assertNoIndexDDL(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	before := deferredIndexOIDs(t, pool)
	resetStatements(t, pool)
	bootWithin(t, pool, startupDeadline)
	for _, stmt := range ddlStatementsSinceReset(t, pool) {
		head := strings.ToUpper(stmt)
		if strings.HasPrefix(head, "CREATE INDEX") || strings.HasPrefix(head, "DROP INDEX") ||
			strings.HasPrefix(head, "CREATE UNIQUE INDEX") {
			t.Fatalf("the boot after the rebuild issued index DDL: %s", stmt)
		}
	}
	if after := deferredIndexOIDs(t, pool); after != before {
		t.Fatalf("the boot after the rebuild replaced a deferred index: %s -> %s",
			before, after)
	}
}

func deferredIndexOIDs(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var oids string
	if err := pool.QueryRow(context.Background(), `SELECT string_agg(n || '=' ||
		COALESCE(to_regclass('sage.' || n)::oid::text, '-'), ',' ORDER BY n)
		FROM unnest($1::text[]) n`, deferredNames()).Scan(&oids); err != nil {
		t.Fatalf("read the deferred index oids: %v", err)
	}
	return oids
}

func deferredNames() []string {
	names := make([]string, 0, len(deferredIndexes))
	for _, idx := range deferredIndexes {
		names = append(names, idx.name)
	}
	return names
}

// Integration (meta and fleet removal): cancelling the builder's context,
// as removing a database does, leaves no build running.
func TestDeferredIndexBuilder_CancelLeavesNoBuildRunning(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_removal")
	mustExec(t, pool, "DROP INDEX sage."+deferredTestIndex)
	_ = interruptBuild(t, pool, func(ctx context.Context) error {
		(&DeferredIndexBuilder{Pool: pool, Database: "removed"}).Run(ctx)
		return nil
	})
	var builds int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND query ILIKE 'CREATE INDEX CONCURRENTLY%'
		  AND state <> 'idle'`).Scan(&builds); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	if builds != 0 {
		t.Fatalf("%d builds still running after the builder was cancelled", builds)
	}
	var lockHeld bool
	if err := pool.QueryRow(context.Background(), `SELECT count(*) > 0 FROM pg_locks
		WHERE locktype = 'advisory' AND objid = hashtext($1)::oid`,
		deferredBuildLockKey).Scan(&lockHeld); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read pg_locks: %v", err)
	}
	if lockHeld {
		t.Fatal("the cancelled builder still holds its advisory lock")
	}
}
