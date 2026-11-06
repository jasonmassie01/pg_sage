package optimizer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

// indexes-05: the HypoPG what-if planned every workload statement twice
// per candidate with no cap, in a session with no lock timeout.

const hypoCandidate = "CREATE INDEX candidate ON hypopg_session_test.items (category)"

// manyHypoQueries returns n plannable statements, hottest first: queryid
// i has total time 1000-i, so the top 25 by total time are 1..25.
func manyHypoQueries(n int) []QueryInfo {
	out := make([]QueryInfo, 0, n)
	for i := 1; i <= n; i++ {
		text := fmt.Sprintf("SELECT id FROM hypopg_session_test.items WHERE category=%d", i)
		out = append(out, QueryInfo{QueryID: int64(i), Calls: 10,
			TotalTimeMs: float64(1000 - i), Text: text})
	}
	return out
}

// Boundary: 200 statements give at most 25 EXPLAIN pairs, and an affected
// queryid outside the top 25 is still measured. The affected statement
// cannot be planned, so its attempt shows in Failed; a cold statement
// that is not affected and cannot be planned is never attempted.
func TestHypoPGCapsStatementsAndKeepsAffected(t *testing.T) {
	pool := hypopgSessionPool(t)
	settleItems(t)
	h := NewHypoPG(pool, noopLog2)
	queries := manyHypoQueries(200)
	queries = append(queries,
		QueryInfo{QueryID: 9999, Calls: 1, TotalTimeMs: 0.5,
			Text: "SELECT missing_affected FROM hypopg_session_test.items WHERE category=1"},
		QueryInfo{QueryID: 9998, Calls: 1, TotalTimeMs: 0.4,
			Text: "SELECT missing_cold FROM hypopg_session_test.items WHERE category=1"})
	rec := Recommendation{DDL: hypoCandidate, AffectedQueryIDs: []int64{9999}}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	res, err := h.Validate(ctx, rec, queries)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if res.Measured+res.Failed > maxWhatIfStatements {
		t.Fatalf("measured %d + failed %d statements, want at most %d",
			res.Measured, res.Failed, maxWhatIfStatements)
	}
	if res.Failed != 1 || res.Measured != maxWhatIfStatements-1 {
		t.Fatalf("result %+v: want the affected statement attempted (1 failed) and "+
			"%d of the hottest measured", res, maxWhatIfStatements-1)
	}
	assertHypoPGSessionClean(t, pool)
}

// settleItems vacuums the freshly loaded fixture table so autovacuum does
// not process it mid-evaluation: its relcache invalidation marks HypoPG's
// hypothetical index invalid, and hypopg_relation_size then returns NULL.
func settleItems(t *testing.T) {
	t.Helper()
	admin := connectTestDB(t)
	defer admin.Close()
	if _, err := admin.Exec(t.Context(), "VACUUM ANALYZE hypopg_session_test.items"); err != nil {
		t.Fatalf("vacuum fixture: %v", err)
	}
}

// lockItems holds ACCESS EXCLUSIVE on the HypoPG fixture table from a
// separate pool until the test ends.
func lockItems(t *testing.T) {
	t.Helper()
	admin := connectTestDB(t)
	t.Cleanup(admin.Close)
	tx, err := admin.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin locker: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(t.Context(),
		"LOCK TABLE hypopg_session_test.items IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock items: %v", err)
	}
}

// Error propagation: a lock wait inside the HypoPG session ends with
// SQLSTATE 55P03 after the session's lock timeout, not after the 5 s
// statement timeout, and the evaluation reports it as an error instead of
// counting the statement as one that could not be planned.
func TestHypoPGLockWaitEndsWithLockTimeout(t *testing.T) {
	pool := hypopgSessionPool(t)
	lockItems(t)
	h := NewHypoPG(pool, noopLog2)
	h.lockTimeout = 200 * time.Millisecond
	queries := []QueryInfo{{QueryID: 1,
		Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}}
	start := time.Now()
	res, err := h.Validate(t.Context(), Recommendation{DDL: hypoCandidate}, queries)
	elapsed := time.Since(start)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("err = %v (result %+v), want SQLSTATE 55P03", err, res)
	}
	if !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("err = %q, want it to say lock timeout", err)
	}
	if res != (WhatIfResult{}) {
		t.Fatalf("a lock-timed-out evaluation produced evidence: %+v", res)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("lock wait took %s, want the 200ms lock timeout", elapsed)
	}
	assertNoWaitingLocks(t)
	assertHypoPGSessionClean(t, pool)
}

// assertNoWaitingLocks: no backend other than the locker's is waiting on
// a relation lock (CHECK-02).
func assertNoWaitingLocks(t *testing.T) {
	t.Helper()
	admin := connectTestDB(t)
	defer admin.Close()
	var n int
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks
		WHERE NOT granted AND locktype = 'relation'`).Scan(&n); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d backends still wait on a relation lock", n)
	}
}

// The optimizer's what-if takes its lock timeout from the safety
// configuration (catalog read timeouts) and its verdict says "lock
// timeout": unverified, never rejected, never a crash.
func TestOptimizerWhatIfReportsLockTimeout(t *testing.T) {
	pool := hypopgSessionPool(t)
	lockItems(t)
	o := New(pool, &config.OptimizerConfig{HypoPGMinImprovePct: 10}, 170000, noopLog2,
		WithCatalogReadTimeouts(catalogread.Timeouts{Statement: time.Second,
			Lock: 200 * time.Millisecond}))
	tc := TableContext{Schema: "hypopg_session_test", Table: "items",
		Queries: []QueryInfo{{QueryID: 1, Calls: 10, TotalTimeMs: 100,
			Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}}}
	rec, rejected := o.enrichWithHypoPG(t.Context(), Recommendation{DDL: hypoCandidate}, tc)
	if rejected || rec.WhatIf != WhatIfUnverified || rec.Validated {
		t.Fatalf("verdict = %s rejected %v validated %v, want unverified",
			rec.WhatIf, rejected, rec.Validated)
	}
	if !strings.Contains(rec.WhatIfReason, "lock timeout") {
		t.Fatalf("reason = %q, want it to say lock timeout", rec.WhatIfReason)
	}
}

// Happy path: without a lock in the way the recommendation's what-if
// still runs and verifies a useful index.
func TestOptimizerWhatIfStillRuns(t *testing.T) {
	pool := hypopgSessionPool(t)
	o := New(pool, &config.OptimizerConfig{HypoPGMinImprovePct: 10}, 170000, noopLog2)
	tc := TableContext{Schema: "hypopg_session_test", Table: "items",
		Queries: []QueryInfo{{QueryID: 1, Calls: 10, TotalTimeMs: 100,
			Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}}}
	rec, rejected := o.enrichWithHypoPG(t.Context(), Recommendation{DDL: hypoCandidate}, tc)
	if rejected || rec.WhatIf != WhatIfVerified || !rec.Validated ||
		rec.EstimatedImprovementPct <= 10 || rec.CostEstimate == nil {
		t.Fatalf("what-if = %+v rejected %v, want a verified improvement", rec, rejected)
	}
}

// The session's lock timeout is the configured one and is LOCAL: it does
// not outlive the session's transaction on the pooled connection.
func TestHypoPGSessionLockTimeoutIsLocal(t *testing.T) {
	pool := hypopgSessionPool(t)
	session, err := openHypoPGSession(t.Context(), pool, 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	var inside, statement string
	if err := session.tx.QueryRow(t.Context(), "SHOW lock_timeout").Scan(&inside); err != nil {
		t.Fatal(err)
	}
	if err := session.tx.QueryRow(t.Context(),
		"SHOW statement_timeout").Scan(&statement); err != nil {
		t.Fatal(err)
	}
	if err := session.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if inside != "1500ms" || statement != "5s" {
		t.Fatalf("session timeouts = lock %s statement %s, want 1500ms and 5s",
			inside, statement)
	}
	var after string
	err = pool.QueryRow(t.Context(), "SHOW lock_timeout").Scan(&after)
	if err != nil || after != "0" {
		t.Fatalf("lock_timeout leaked to the pooled connection: %q %v", after, err)
	}
}

// A zero lock timeout must not mean "wait forever": the session falls
// back to the shipped safety bound.
func TestHypoPGSessionZeroLockTimeoutFailsClosed(t *testing.T) {
	pool := hypopgSessionPool(t)
	session, err := openHypoPGSession(t.Context(), pool, 0)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = session.close() }()
	var got string
	if err := session.tx.QueryRow(t.Context(), "SHOW lock_timeout").Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%ds", catalogread.Default().Lock/time.Second)
	if got != want {
		t.Fatalf("lock_timeout = %s, want the default %s", got, want)
	}
}
