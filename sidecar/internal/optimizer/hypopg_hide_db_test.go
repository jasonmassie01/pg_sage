package optimizer

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// indexes-07 on real PostgreSQL with HypoPG 1.4+: a replacement is
// measured with the old index hidden, a covered drop is measured by hiding
// the index, and the hidden state never outlives the session. The tests
// run on the package's database of the shared server, except the one whose
// verdict needs an all-visible heap (newExclusiveHideFixture).

// hideFixture is a table with a real index items_a on (a) and a true
// duplicate items_a_dup, vacuumed and analyzed.
type hideFixture struct {
	pool              *pgxpool.Pool
	itemsA, itemsDup  uint32
	table             uint32
	pointLookup, pair string
}

// newHideFixture builds the fixture on the shared server with one VACUUM
// ANALYZE. Its tests' verdicts hold whether or not that VACUUM could mark
// the heap all-visible, though the point lookup's plan and cost change with it.
func newHideFixture(t *testing.T) hideFixture {
	t.Helper()
	pool := hypopgSessionPool(t)
	admin := connectTestDB(t)
	defer admin.Close()
	f := seedHideFixture(t, pool, admin)
	if _, err := admin.Exec(t.Context(), "VACUUM ANALYZE hypopg_hide_test.items"); err != nil {
		t.Fatalf("vacuum the fixture: %v", err)
	}
	return f
}

// newExclusiveHideFixture builds the fixture on a fresh database of the
// exclusive server, held for t alone (testdb.Exclusive), and vacuums until
// every heap page is all-visible, so "SELECT a" is planned as an
// index-only scan. On the shared server PostgreSQL 14 counts a backend
// still logging in to any database in this database's horizon, back to the
// oldest transaction running there, so a VACUUM can leave every page unmarked.
func newExclusiveHideFixture(t *testing.T, reason string) hideFixture {
	t.Helper()
	admin := connectExclusiveTestDB(t, reason)
	f := seedHideFixture(t, hypopgSessionPoolOn(t, admin), admin)
	err := testdb.VacuumAllVisible(t.Context(), admin, "hypopg_hide_test.items",
		60*time.Second)
	if err != nil {
		t.Fatalf("all-visible fixture: %v", err)
	}
	return f
}

// connectExclusiveTestDB is connectTestDB on a fresh database of the
// exclusive server, closed before that database is dropped.
func connectExclusiveTestDB(t *testing.T, reason string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.Exclusive(t, reason))
	if err != nil {
		t.Fatalf("connect to the exclusive test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping the exclusive test database: %v", err)
	}
	return pool
}

// seedHideFixture creates the table and its two indexes through admin,
// unvacuumed, and returns the fixture on the session pool.
func seedHideFixture(t *testing.T, pool, admin *pgxpool.Pool) hideFixture {
	t.Helper()
	for _, sql := range []string{
		"DROP SCHEMA IF EXISTS hypopg_hide_test CASCADE",
		"CREATE SCHEMA hypopg_hide_test",
		"CREATE TABLE hypopg_hide_test.items (id int, a int, b int, big_text text)",
		"INSERT INTO hypopg_hide_test.items SELECT i, i%100, i%1000, " +
			"repeat(md5(i::text), 10) FROM generate_series(1,20000) i",
		"CREATE INDEX items_a ON hypopg_hide_test.items (a)",
		"CREATE INDEX items_a_dup ON hypopg_hide_test.items (a)",
	} {
		if _, err := admin.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	f := hideFixture{pool: pool,
		pointLookup: "SELECT a FROM hypopg_hide_test.items WHERE a = 42",
		pair:        "SELECT * FROM hypopg_hide_test.items WHERE a = 42 AND b = 42"}
	err := admin.QueryRow(t.Context(), `SELECT 'hypopg_hide_test.items_a'::regclass::oid,
		'hypopg_hide_test.items_a_dup'::regclass::oid,
		'hypopg_hide_test.items'::regclass::oid`).Scan(&f.itemsA, &f.itemsDup, &f.table)
	if err != nil {
		t.Fatalf("read fixture OIDs: %v", err)
	}
	return f
}

// dropDup removes the duplicate, so items_a is the only index on (a).
func (f hideFixture) dropDup(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), "DROP INDEX hypopg_hide_test.items_a_dup"); err != nil {
		t.Fatalf("drop duplicate: %v", err)
	}
}

func assertNothingHidden(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*)
		FROM hypopg_session_test.hypopg_hidden_indexes()`).Scan(&n); err != nil {
		t.Fatalf("read hidden indexes: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d indexes are still hidden on the pooled connection", n)
	}
}

func hideOptimizer(pool *pgxpool.Pool) *Optimizer {
	return New(pool, &config.OptimizerConfig{HypoPGMinImprovePct: 10}, noopLog2)
}

func backendPID(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var pid int
	if err := pool.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read backend pid: %v", err)
	}
	return pid
}

func TestHypoPGSessionDetectsHideSupport(t *testing.T) {
	f := newHideFixture(t)
	s, err := openHypoPGSession(t.Context(), f.pool, 0)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	canHide := s.canHide
	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !canHide {
		t.Fatal("HypoPG 1.4+ installed but hypopg_hide_index not detected")
	}
}

// Replacing (a) with the much wider (a, big_text) makes the hot point
// lookup costlier once (a) is hidden: rejected, naming the queryid. The
// regression is the index-only scan of (a) against the wide index, so the
// heap must be all-visible. Without that both plans are bitmap heap scans
// under 10% apart, below the guarded-regression bar, so the rejection is
// only for missing the 10% improvement minimum and names neither the
// queryid nor items_a.
func TestHypoPGReplacementRejectsGuardedRegression(t *testing.T) {
	f := newExclusiveHideFixture(t,
		"the point lookup's index-only scan needs an all-visible heap")
	f.dropDup(t)
	tc := TableContext{Schema: "hypopg_hide_test", Table: "items",
		Queries: []QueryInfo{{QueryID: 4101, Calls: 1000, TotalTimeMs: 5000,
			Text: f.pointLookup}}}
	rec := Recommendation{DDL: "CREATE INDEX items_wide ON hypopg_hide_test.items " +
		"(a, big_text)", Replaces: &IndexRef{Name: "items_a", OID: f.itemsA}}
	rec, rejected := hideOptimizer(f.pool).enrichWithHypoPG(t.Context(), rec, tc)
	if !rejected || rec.WhatIf != WhatIfRejected || rec.Validated {
		t.Fatalf("verdict %s rejected %v, want rejected: %s", rec.WhatIf, rejected,
			rec.WhatIfReason)
	}
	if !strings.Contains(rec.WhatIfReason, "queryid 4101") ||
		!strings.Contains(rec.WhatIfReason, "items_a") {
		t.Fatalf("reason %q must name queryid 4101 and items_a", rec.WhatIfReason)
	}
	assertHypoPGSessionClean(t, f.pool)
	assertNothingHidden(t, f.pool)
}

// Replacing (a) with (a, b): the pair lookup gains, the point lookup that
// used (a) costs about the same once (a) is hidden. Verified.
func TestHypoPGReplacementWithoutRegressionVerifies(t *testing.T) {
	f := newHideFixture(t)
	f.dropDup(t)
	queries := []QueryInfo{
		{QueryID: 1, Calls: 1000, TotalTimeMs: 9000, Text: f.pair},
		{QueryID: 2, Calls: 1000, TotalTimeMs: 1000,
			Text: "SELECT * FROM hypopg_hide_test.items WHERE a = 42"}}
	rec := Recommendation{DDL: "CREATE INDEX items_ab ON hypopg_hide_test.items (a, b)",
		Replaces: &IndexRef{Name: "items_a", OID: f.itemsA}}
	res, err := NewHypoPG(f.pool, noopLog2).Validate(t.Context(), rec, queries)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if res.Guarded != 2 || len(res.Regressions) != 0 || res.HiddenIndex != "items_a" ||
		res.OldIndexPresent || res.Measured != 2 || res.Improvement <= 10 {
		t.Fatalf("result %+v, want 2 guarded, no regression, a real improvement", res)
	}
	if verdict, reason := whatIfVerdict(res, nil, 10); verdict != WhatIfVerified {
		t.Fatalf("verdict %s %q, want verified", verdict, reason)
	}
	assertHypoPGSessionClean(t, f.pool)
	assertNothingHidden(t, f.pool)
}

// A true duplicate's drop measures 0%: the planner uses the twin.
func TestHypoPGDuplicateDropMeasuresZero(t *testing.T) {
	f := newHideFixture(t)
	tc := TableContext{Schema: "hypopg_hide_test", Table: "items",
		Queries: []QueryInfo{{QueryID: 7, Calls: 100, TotalTimeMs: 100, Text: f.pointLookup},
			{QueryID: 8, Calls: 100, TotalTimeMs: 50,
				Text: "SELECT * FROM hypopg_hide_test.items WHERE a = 42"}}}
	o := hideOptimizer(f.pool)
	for _, ix := range []IndexRef{{Name: "items_a", OID: f.itemsA},
		{Name: "items_a_dup", OID: f.itemsDup}} {
		m := o.MeasureDrop(t.Context(), ix, tc)
		if m.Verdict != WhatIfVerified || m.ChangePct != 0 || m.Measured != 2 ||
			len(m.Regressions) != 0 {
			t.Fatalf("drop of %s = %+v, want verified at 0%%", ix.Name, m)
		}
	}
	assertHypoPGSessionClean(t, f.pool)
	assertNothingHidden(t, f.pool)
}

// The only index on (a) is not covered: hiding it regresses the lookup,
// so its drop is rejected naming the queryid.
func TestHypoPGDropOfTheOnlyIndexRegresses(t *testing.T) {
	f := newHideFixture(t)
	f.dropDup(t)
	tc := TableContext{Queries: []QueryInfo{{QueryID: 7, Calls: 100, TotalTimeMs: 100,
		Text: f.pointLookup}}}
	m := hideOptimizer(f.pool).MeasureDrop(t.Context(),
		IndexRef{Name: "items_a", OID: f.itemsA}, tc)
	if m.Verdict != WhatIfRejected || !strings.Contains(m.Reason, "queryid 7") ||
		m.Guarded != 1 || len(m.Regressions) != 1 {
		t.Fatalf("drop = %+v, want rejected naming queryid 7", m)
	}
	assertNothingHidden(t, f.pool)
}

// Error propagation: a hide that fails (the OID is the table's, not an
// index's) gives unverified, and the backend is never pooled again.
func TestHypoPGFailingHideIsUnverifiedAndNotPooled(t *testing.T) {
	f := newHideFixture(t)
	before := backendPID(t, f.pool)
	rec := Recommendation{DDL: "CREATE INDEX items_ab ON hypopg_hide_test.items (a, b)",
		Replaces: &IndexRef{Name: "items", OID: f.table}}
	tc := TableContext{Queries: []QueryInfo{{QueryID: 1, Calls: 10, TotalTimeMs: 10,
		Text: f.pair}}}
	rec, rejected := hideOptimizer(f.pool).enrichWithHypoPG(t.Context(), rec, tc)
	if rejected || rec.WhatIf != WhatIfUnverified || rec.Validated {
		t.Fatalf("verdict %s rejected %v, want unverified", rec.WhatIf, rejected)
	}
	if !strings.Contains(rec.WhatIfReason, "hide") {
		t.Fatalf("reason %q does not say the hide failed", rec.WhatIfReason)
	}
	if after := backendPID(t, f.pool); after == before {
		t.Fatalf("backend %d with a failed hide went back to the pool", before)
	}
	if f.pool.Stat().AcquiredConns() != 0 {
		t.Fatal("failed session leaked a checkout")
	}
	assertNothingHidden(t, f.pool)
}

// A replaced index that no longer exists is an error, not a hide of
// nothing: the what-if is unverified.
func TestHypoPGReplacedIndexGoneIsUnverified(t *testing.T) {
	f := newHideFixture(t)
	f.dropDup(t)
	rec := Recommendation{DDL: "CREATE INDEX items_ab ON hypopg_hide_test.items (a, b)",
		Replaces: &IndexRef{Name: "items_a_dup", OID: f.itemsDup}}
	res, err := NewHypoPG(f.pool, noopLog2).Validate(t.Context(), rec,
		[]QueryInfo{{QueryID: 1, Text: f.pair}})
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("validate = %+v %v, want the replaced index named as gone", res, err)
	}
	assertHypoPGSessionClean(t, f.pool)
}

// State: a hide inside the session is undone by close on the same
// backend, which then goes back to the pool.
func TestHypoPGCloseUnhidesOnTheSameBackend(t *testing.T) {
	f := newHideFixture(t)
	before := backendPID(t, f.pool)
	s, err := openHypoPGSession(t.Context(), f.pool, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.hide(t.Context(), f.itemsA); err != nil {
		t.Fatalf("hide: %v", err)
	}
	var hidden int
	err = s.tx.QueryRow(t.Context(), `SELECT count(*)
		FROM hypopg_session_test.hypopg_hidden_indexes()`).Scan(&hidden)
	if err != nil || hidden != 1 {
		t.Fatalf("hidden inside the session = %d (%v), want 1", hidden, err)
	}
	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if after := backendPID(t, f.pool); after != before {
		t.Fatalf("a clean session was discarded: pid %d then %d", before, after)
	}
	assertNothingHidden(t, f.pool)
}

// Concurrency: two parallel sessions; the index one of them hides is still
// planned by the other.
func TestHypoPGHiddenIndexIsSessionLocal(t *testing.T) {
	f := newHideFixture(t)
	f.dropDup(t)
	cfg := f.pool.Config().Copy()
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	hider, err := openHypoPGSession(t.Context(), pool, 0)
	if err != nil {
		t.Fatalf("open hider: %v", err)
	}
	other, err := openHypoPGSession(t.Context(), pool, 0)
	if err != nil {
		t.Fatalf("open other: %v", errJoinClose(err, hider))
	}
	if err := hider.hide(t.Context(), f.itemsA); err != nil {
		t.Fatalf("hide: %v", err)
	}
	uses := make([]bool, 2)
	var wg sync.WaitGroup
	for i, s := range []*hypopgSession{hider, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plan, err := s.explain(context.Background(), f.pointLookup)
			if err != nil {
				t.Errorf("explain in session %d: %v", i, err)
				return
			}
			uses[i] = planhash.UsesIndex(plan, "items_a")
		}()
	}
	wg.Wait()
	if err := hider.close(); err != nil {
		t.Fatalf("close hider: %v", err)
	}
	if err := other.close(); err != nil {
		t.Fatalf("close other: %v", err)
	}
	if uses[0] || !uses[1] {
		t.Fatalf("items_a used: hiding session %v, other session %v; want false, true",
			uses[0], uses[1])
	}
}

func errJoinClose(err error, s *hypopgSession) error {
	if closeErr := s.close(); closeErr != nil {
		return closeErr
	}
	return err
}

// Without hide support today's measurement stands (old index present): no
// statement is guarded and the verdict's reason names the HypoPG version.
func TestHypoPGWithoutHideMeasuresWithTheOldIndex(t *testing.T) {
	f := newHideFixture(t)
	f.dropDup(t)
	s, err := openHypoPGSession(t.Context(), f.pool, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.canHide = false
	rec := Recommendation{DDL: "CREATE INDEX items_wide ON hypopg_hide_test.items " +
		"(a, big_text)", Replaces: &IndexRef{Name: "items_a", OID: f.itemsA}}
	res, err := s.evaluate(t.Context(), rec, []QueryInfo{{QueryID: 4101, Calls: 1000,
		TotalTimeMs: 5000, Text: f.pointLookup}})
	if closeErr := s.close(); closeErr != nil {
		t.Fatalf("close: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.OldIndexPresent || res.Guarded != 0 || len(res.Regressions) != 0 ||
		res.HiddenIndex != "" || res.Measured != 1 {
		t.Fatalf("result %+v, want today's measurement with the old index present", res)
	}
	_, reason := whatIfVerdict(res, nil, 10)
	if !strings.Contains(reason, hideUnsupportedReason) {
		t.Fatalf("reason %q, want %q", reason, hideUnsupportedReason)
	}
	assertNothingHidden(t, f.pool)
}
