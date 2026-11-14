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

// config-04c on a real server: interrupted and running background builds
// (shared helpers are in deferred_index_db_test.go).

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

// cancelledBuildStopsWithin bounds how long a cancelled build may stay on
// the server: well under migrationLockTimeout, which would end it anyway.
const cancelledBuildStopsWithin = migrationLockTimeout * 3 / 4

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
	cancelled := time.Now()
	cancel()
	select {
	case err := <-done:
		waitFor(t, pool, noBuildRunning)
		// Without a cancel request the server keeps waiting until its 2 s
		// lock_timeout; the cancel must stop the build at once.
		if gone := time.Since(cancelled); gone > cancelledBuildStopsWithin {
			t.Fatalf("the build ran on for %s after the cancel: the server was not "+
				"sent a cancel request", gone)
		}
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

// State transition: while a build waits on an older writer, its index is
// counted building, not invalid; afterwards nothing is pending.
func TestReadDeferredIndexStates_CountsARunningBuild(t *testing.T) {
	pool := freshSchemaPool(t, "deferred_states")
	mustExec(t, pool, "DROP INDEX sage."+deferredTestIndex)
	counts, err := ReadDeferredIndexStates(context.Background(), pool)
	if err != nil || counts != (DeferredIndexCounts{Missing: 1}) {
		t.Fatalf("before the build: %+v, %v; want missing=1", counts, err)
	}
	release := openWriter(t, pool)
	done := make(chan error, 1)
	go func() {
		_, err := BuildDeferredIndexes(context.Background(), pool)
		done <- err
	}()
	waitFor(t, pool, cicWaitingOnWriter)
	counts, err = ReadDeferredIndexStates(context.Background(), pool)
	release()
	if err != nil || counts != (DeferredIndexCounts{Building: 1}) {
		t.Fatalf("during the build: %+v, %v; want building=1", counts, err)
	}
	if err := <-done; err != nil { // the writer may outlast lock_timeout
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != sqlLockNotAvailable {
			t.Fatalf("build: %v", err)
		}
		if _, err := BuildDeferredIndexes(context.Background(), pool); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
	}
	counts, err = ReadDeferredIndexStates(context.Background(), pool)
	if err != nil || counts != (DeferredIndexCounts{}) {
		t.Fatalf("after the build: %+v, %v; want zero", counts, err)
	}
}
