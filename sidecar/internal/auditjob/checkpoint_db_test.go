package auditjob

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// freshPair bootstraps a database and returns a pool on it plus a second
// pool whose sessions run with a short statement_timeout.
func freshPair(t *testing.T, label string) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "1500"
	slow, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with statement_timeout: %v", err)
	}
	t.Cleanup(slow.Close)
	return pool, slow
}

// openOn reports the open finding of category on audit_trail:<chain>.
func openOn(t *testing.T, pool *pgxpool.Pool, category, chain string) (bool, string) {
	t.Helper()
	var n int
	var sev string
	err := pool.QueryRow(context.Background(), `SELECT count(*), coalesce(max(severity),'')
		FROM sage.findings WHERE category = $1 AND object_identifier = $2
		AND status = 'open'`, category, "audit_trail:"+chain).Scan(&n, &sev)
	if err != nil {
		t.Fatalf("read %s findings: %v", category, err)
	}
	if n > 1 {
		t.Fatalf("%d open %s findings on %s, want at most one", n, category, chain)
	}
	return n == 1, sev
}

func reportOf(reps []auditchain.Report, chain string) (auditchain.Report, bool) {
	for _, r := range reps {
		if r.Chain == chain {
			return r, true
		}
	}
	return auditchain.Report{}, false
}

// Error propagation: a statement_timeout on chain 2 (auth_audit, locked
// by another session) still raises chain 1's finding, gives chain 2 a
// verification_failed warning, and keeps chain 2's earlier finding open.
func TestFailingChainDoesNotStopTheOthers(t *testing.T) {
	pool, slow := freshPair(t, "auditjob_timeout")
	ctx := context.Background()
	tamperTwoChains(t, pool)
	lock := lockTable(t, pool, "sage.auth_audit")
	reports, err := Run(ctx, Source{Name: "orders", Pool: slow})
	if err == nil || !strings.Contains(err.Error(), "auth_audit") {
		t.Fatalf("run with chain 2 timing out: err = %v, want one naming auth_audit", err)
	}
	if rep, ok := reportOf(reports, "action_log"); !ok || rep.OK() {
		t.Fatalf("chain 1 report missing or clean after its tampering: %+v", reports)
	}
	if _, ok := reportOf(reports, "config_audit"); !ok {
		t.Fatalf("chain 3 not verified after chain 2 failed: %+v", reports)
	}
	if open, _ := openOn(t, pool, FindingCategory, "action_log"); !open {
		t.Fatalf("chain 1's finding was not raised while chain 2 failed")
	}
	if open, _ := openOn(t, pool, FindingCategory, "auth_audit"); !open {
		t.Fatalf("chain 2's finding was resolved by a run that could not verify it")
	}
	open, sev := openOn(t, pool, FailedCategory, "auth_audit")
	if !open || sev != "warning" {
		t.Fatalf("verification_failed on auth_audit: open=%v severity=%q, want a warning",
			open, sev)
	}
	if open, _ := openOn(t, pool, FailedCategory, "action_log"); open {
		t.Fatalf("verification_failed raised on a chain that verified")
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	if _, err := Run(ctx, Source{Name: "orders", Pool: pool}); err != nil {
		t.Fatalf("run after the lock: %v", err)
	}
	if open, _ := openOn(t, pool, FailedCategory, "auth_audit"); open {
		t.Fatalf("verification_failed stays open after chain 2 verified again")
	}
	if open, _ := openOn(t, pool, FindingCategory, "auth_audit"); !open {
		t.Fatalf("chain 2's tampering finding resolved although it is still tampered")
	}
}

// tamperTwoChains tampers auth_audit, raises its finding with one run, then
// tampers action_log.
func tamperTwoChains(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO sage.auth_audit (event, detail) VALUES ('login', '{}')`)
	mustExec(t, pool, `UPDATE sage.auth_audit SET event = 'tampered'`)
	if _, err := Run(context.Background(), Source{Name: "orders", Pool: pool}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if open, _ := openOn(t, pool, FindingCategory, "auth_audit"); !open {
		t.Fatalf("tampered auth_audit raised no finding")
	}
	mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM a')`)
	mustExec(t, pool, `UPDATE sage.action_log SET sql_executed = 'SELECT 1'`)
}

// lockTable holds an ACCESS EXCLUSIVE lock on table until the returned
// transaction ends (or the test does).
func lockTable(t *testing.T, pool *pgxpool.Pool, table string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Rollback(ctx) })
	if _, err := lock.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock %s: %v", table, err)
	}
	return lock
}

func checkpointCount(t *testing.T, pool *pgxpool.Pool, chain string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*)
		FROM sage.audit_chain_checkpoint WHERE chain = $1`, chain).Scan(&n); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	return n
}

func runMode(t *testing.T, src Source, chain string) auditchain.Report {
	t.Helper()
	reports, err := Run(context.Background(), src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rep, ok := reportOf(reports, chain)
	if !ok {
		t.Fatalf("no %s report in %+v", chain, reports)
	}
	return rep
}

// State transitions: no checkpoint, then full; then incremental from the
// stored checkpoint (blind to an edit before it); then full again once
// the interval has elapsed, which finds the edit and stores nothing.
func TestCheckpointModeLifecycle(t *testing.T) {
	pool := fresh(t, "auditjob_modes", true)
	src := Source{Name: "orders", Pool: pool, FullVerifyHours: 168}
	mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM a')`)
	rep := runMode(t, src, "action_log")
	if rep.Mode != auditchain.ModeFull || !rep.OK() || checkpointCount(t, pool,
		"action_log") != 1 {
		t.Fatalf("first run: %+v, want a clean full pass and one checkpoint", rep)
	}
	if n := checkpointCount(t, pool, "pgaudit_events"); n != 1 {
		t.Fatalf("empty chain stored %d checkpoints, want one at 0", n)
	}
	first := rep.HeadSeq
	mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM b')`)
	rep = runMode(t, src, "action_log")
	if rep.Mode != auditchain.ModeIncremental || rep.FromSeq != first+1 || !rep.OK() {
		t.Fatalf("second run: %+v, want incremental from %d", rep, first+1)
	}
	mustExec(t, pool, "UPDATE sage.action_log SET sql_executed = 'SELECT 1' "+
		"WHERE sql_executed = 'VACUUM a'")
	if rep = runMode(t, src, "action_log"); rep.Mode != auditchain.ModeIncremental ||
		!rep.OK() {
		t.Fatalf("incremental after an edit before the checkpoint: %+v", rep)
	}
	stored := checkpointCount(t, pool, "action_log")
	mustExec(t, pool, `UPDATE sage.audit_chain_checkpoint
		SET verified_at = now() - interval '169 hours' WHERE mode = 'full'`)
	rep = runMode(t, src, "action_log")
	if rep.Mode != auditchain.ModeFull || rep.OK() {
		t.Fatalf("run after the interval: %+v, want a full pass finding the edit", rep)
	}
	if n, _, _ := openFindings(t, pool); n != 1 {
		t.Fatalf("full pass raised %d findings, want 1", n)
	}
	if n := checkpointCount(t, pool, "action_log"); n != stored {
		t.Fatalf("a failing pass stored a checkpoint (%d, was %d)", n, stored)
	}
	if rep = runMode(t, src, "action_log"); rep.Mode != auditchain.ModeFull || rep.OK() {
		t.Fatalf("next run: %+v, want full again while the trail fails", rep)
	}
}

// Boundary: an interval of 0 hours makes every pass full.
func TestZeroIntervalAlwaysVerifiesInFull(t *testing.T) {
	pool := fresh(t, "auditjob_always_full", true)
	src := Source{Name: "orders", Pool: pool}
	for i := 0; i < 3; i++ {
		mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
			VALUES ('vacuum', 'VACUUM z')`)
		if rep := runMode(t, src, "action_log"); rep.Mode != auditchain.ModeFull ||
			rep.FromSeq != 1 || !rep.OK() {
			t.Fatalf("run %d with interval 0: %+v, want a clean full pass", i, rep)
		}
	}
}
