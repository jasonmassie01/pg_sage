package analyzer

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// G2-B13: INCLUDE and WHERE clauses must not be swallowed into the key
// column list.
func TestRegression_ParseIndexDefIncludeAndWhere(t *testing.T) {
	p := ParseIndexDef(
		"CREATE INDEX i ON public.t USING btree (a) INCLUDE (b, c)")
	if !reflect.DeepEqual(p.Columns, []string{"a"}) ||
		!reflect.DeepEqual(p.IncludeCols, []string{"b", "c"}) {
		t.Fatalf("INCLUDE parse: cols=%q include=%q", p.Columns, p.IncludeCols)
	}
	p = ParseIndexDef(
		"CREATE INDEX i ON public.t USING btree (a, b) WHERE (deleted_at IS NULL)")
	if !reflect.DeepEqual(p.Columns, []string{"a", "b"}) ||
		p.WhereClause != "(deleted_at IS NULL)" {
		t.Fatalf("WHERE parse: cols=%q where=%q", p.Columns, p.WhereClause)
	}
	p = ParseIndexDef("CREATE INDEX i ON public.t USING btree (lower((email)::text)) " +
		"INCLUDE (id) WHERE ((a > 1) AND (b < 2))")
	if !reflect.DeepEqual(p.Columns, []string{"lower((email)::text)"}) ||
		!reflect.DeepEqual(p.IncludeCols, []string{"id"}) ||
		p.WhereClause != "((a > 1) AND (b < 2))" {
		t.Fatalf("expression parse: %+v", p)
	}
}

// G2-B13: a full index is not a "subset" of a partial index.
func TestRegression_FullIndexNotSubsetOfPartial(t *testing.T) {
	snap := &collector.Snapshot{Indexes: []collector.IndexStats{
		{SchemaName: "public", RelName: "t", IndexRelName: "i_full", IsValid: true,
			IndexDef: "CREATE INDEX i_full ON public.t USING btree (a)"},
		{SchemaName: "public", RelName: "t", IndexRelName: "i_part", IsValid: true,
			IndexDef: "CREATE INDEX i_part ON public.t USING btree (a, b) " +
				"WHERE (deleted_at IS NULL)"},
	}}
	if got := ruleDuplicateIndexes(snap, nil, nil, nil); len(got) != 0 {
		t.Fatalf("full index reported against partial: %+v", got)
	}
}

func mixedCaseIndex(name string, scans int64, def string) collector.IndexStats {
	return collector.IndexStats{
		SchemaName: "Sales", RelName: "Orders", IndexRelName: name,
		IsValid: true, IdxScan: scans, IndexDef: def,
	}
}

// G2-B22/G4-B23/C16: every DDL-building index rule must quote identifiers.
func TestRegression_IndexDDLQuotesIdentifiers(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	unused := &collector.Snapshot{Indexes: []collector.IndexStats{mixedCaseIndex(
		"IdxFoo", 0, `CREATE INDEX "IdxFoo" ON "Sales"."Orders" USING btree (x)`)}}
	extras := &RuleExtras{
		FirstSeen:       map[string]time.Time{"Sales.IdxFoo": old},
		RecentlyCreated: map[string]time.Time{},
	}
	assertSQL(t, ruleUnusedIndexes(unused, nil, unusedCfg(), extras),
		`DROP INDEX CONCURRENTLY "Sales"."IdxFoo";`)

	invalid := &collector.Snapshot{Indexes: []collector.IndexStats{mixedCaseIndex(
		"IdxBad", 0, `CREATE INDEX "IdxBad" ON "Sales"."Orders" USING btree (x)`)}}
	invalid.Indexes[0].IsValid = false
	invExtras := &RuleExtras{
		InvalidFirstSeen: map[string]time.Time{"Sales.IdxBad": old},
		IndexBuildTables: map[string]bool{},
	}
	assertSQL(t, ruleInvalidIndexes(invalid, nil, nil, invExtras),
		`DROP INDEX CONCURRENTLY "Sales"."IdxBad";`)

	dup := &collector.Snapshot{Indexes: []collector.IndexStats{
		mixedCaseIndex("IdxA", 5, `CREATE INDEX "IdxA" ON "Sales"."Orders" USING btree (x)`),
		mixedCaseIndex("IdxB", 1, `CREATE INDEX "IdxB" ON "Sales"."Orders" USING btree (x)`),
	}}
	assertSQL(t, ruleDuplicateIndexes(dup, nil, nil, nil),
		`DROP INDEX CONCURRENTLY "Sales"."IdxB";`)

	subset := &collector.Snapshot{Indexes: []collector.IndexStats{
		mixedCaseIndex("IdxX", 1, `CREATE INDEX "IdxX" ON "Sales"."Orders" USING btree (x)`),
		mixedCaseIndex("IdxXY", 1, `CREATE INDEX "IdxXY" ON "Sales"."Orders" USING btree (x, y)`),
	}}
	assertSQL(t, ruleDuplicateIndexes(subset, nil, nil, nil),
		`DROP INDEX CONCURRENTLY "Sales"."IdxX";`)

	fkS := &collector.Snapshot{
		Tables: []collector.TableStats{{SchemaName: "Sales", RelName: "Orders"}},
		ForeignKeys: []collector.ForeignKey{{
			TableName: "Orders", ReferencedTable: "Customers",
			FKColumn: "CustomerId", ConstraintName: "orders_fk",
		}},
	}
	assertSQL(t, ruleMissingFKIndexes(fkS, nil, nil, nil),
		`CREATE INDEX CONCURRENTLY ON "Sales"."Orders" ("CustomerId");`)
}

func assertSQL(t *testing.T, ff []Finding, want string) {
	t.Helper()
	if len(ff) != 1 {
		t.Fatalf("findings = %d, want 1 (want SQL %s)", len(ff), want)
	}
	if ff[0].RecommendedSQL != want {
		t.Fatalf("RecommendedSQL = %q, want %q", ff[0].RecommendedSQL, want)
	}
}

// G2-B23: a zero slow-query threshold must fall back to the default
// instead of flagging every query as +Inf x critical.
func TestRegression_SlowQueryZeroThreshold(t *testing.T) {
	cfg := phase2Config()
	cfg.Analyzer.SlowQueryThresholdMs = 0
	snap := &collector.Snapshot{Queries: []collector.QueryStats{
		{QueryID: 1, Query: "SELECT 1", MeanExecTime: 5, Calls: 10},
		{QueryID: 2, Query: "SELECT 2", MeanExecTime: 3000, Calls: 10},
	}}
	got := ruleSlowQueries(snap, nil, cfg, nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "queryid:2" {
		t.Fatalf("findings = %+v, want only queryid:2", got)
	}
	if strings.Contains(got[0].Title, "Inf") || got[0].Severity != "warning" {
		t.Fatalf("title=%q severity=%q", got[0].Title, got[0].Severity)
	}
}

// C18/G1-B35: a descending sequence approaching its minimum must be
// reported even though the collector's pct_used is 0 for it.
func TestRegression_DescendingSequenceExhaustion(t *testing.T) {
	snap := &collector.Snapshot{Sequences: []collector.SequenceStats{{
		SchemaName: "public", SequenceName: "down_seq", DataType: "integer",
		LastValue: -2_000_000_000, MaxValue: -1, IncrementBy: -1, PctUsed: 0,
	}}}
	got := ruleSequenceExhaustion(snap, nil, nil, nil)
	if len(got) != 1 || got[0].Severity != "critical" {
		t.Fatalf("descending sequence findings = %+v, want 1 critical", got)
	}
	asc := &collector.Snapshot{Sequences: []collector.SequenceStats{{
		SchemaName: "public", SequenceName: "up_seq", DataType: "integer",
		LastValue: 1_700_000_000, MaxValue: 2_147_483_647, IncrementBy: 1,
		PctUsed: 79.16,
	}}}
	got = ruleSequenceExhaustion(asc, nil, nil, nil)
	if len(got) != 1 || got[0].Severity != "warning" {
		t.Fatalf("ascending control findings = %+v, want 1 warning", got)
	}
}

// G1-B06: the regression baseline must read the field name the collector
// actually persists (collector.QueryStats → "mean_exec_time").
func TestRegression_HistoricalAveragesReadCollectorField(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanSnapshots(t, pool)
	ctx := context.Background()
	t.Cleanup(func() { phase2CleanSnapshots(t, pool) })
	for _, mean := range []float64{10, 30} {
		data, err := json.Marshal([]collector.QueryStats{{
			QueryID: 4242, Query: "SELECT phase2_test", MeanExecTime: mean,
		}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO sage.snapshots (category, data) VALUES ('queries', $1)`,
			data); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	avg := a.buildHistoricalAverages(ctx)[4242]
	if avg < 19.9 || avg > 20.1 {
		t.Fatalf("historical avg = %v, want 20", avg)
	}
}

// G2-B07: the analyzer loads a real stats epoch from the catalog.
func TestRegression_LoadStatsEpoch(t *testing.T) {
	pool := phase2Pool(t)
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.loadStatsEpoch(context.Background())
	epoch := a.extras.StatsEpoch
	if epoch.IsZero() || epoch.After(time.Now()) {
		t.Fatalf("stats epoch = %v, want a past instant", epoch)
	}
	var started time.Time
	if err := pool.QueryRow(context.Background(),
		"SELECT pg_postmaster_start_time()").Scan(&started); err != nil {
		t.Fatalf("postmaster start: %v", err)
	}
	if epoch.Before(started) {
		t.Fatalf("epoch %v before postmaster start %v", epoch, started)
	}
}

// G2-B09: the build probe runs against the catalog and reports success.
func TestRegression_LoadIndexBuilds(t *testing.T) {
	pool := phase2Pool(t)
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.extras.IndexBuildProbeFailed = true
	a.loadIndexBuilds(context.Background())
	if a.extras.IndexBuildProbeFailed || a.extras.IndexBuildTables == nil {
		t.Fatalf("probe failed=%v tables=%v", a.extras.IndexBuildProbeFailed,
			a.extras.IndexBuildTables)
	}
}

// G2-B28: hint counts per role must count distinct queries of this
// database, not every pg_stat_statements/hint row that joins.
// pg_stat_statements is cluster-wide: another package's test may reset it
// between the queryid lookup and the check. A run whose marker vanished is
// contaminated and is repeated, never scored.
func TestRegression_WorkMemPromotionCountsDistinctQueries(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		"CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Skipf("pg_stat_statements unavailable in fixture: %v", err)
	}
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.query_hints WHERE symptom = 'b28_test'`)
	}
	t.Cleanup(cleanup)
	var role string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatalf("current_user: %v", err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		cleanup()
		qid := seedB28Marker(t, pool)
		count, found := b28HintCount(ctx, t, pool, role)
		if found {
			if count != 1 {
				t.Fatalf("hint_count = %v, want 1 (one distinct query)", count)
			}
			return
		}
		if b28MarkerTracked(ctx, t, pool, qid) {
			t.Fatalf("no work_mem promotion finding for role %s", role)
		}
		t.Logf("attempt %d: pg_stat_statements reset removed the marker; repeating",
			attempt)
	}
	t.Fatalf("pg_stat_statements was reset during every attempt")
}

// seedB28Marker runs the marker query twice and inserts two hints for it.
func seedB28Marker(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, "SELECT 424242 AS b28_marker"); err != nil {
			t.Fatalf("marker query: %v", err)
		}
	}
	var qid int64
	err := pool.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements
		WHERE query LIKE '%b28_marker%' AND dbid = (SELECT oid FROM pg_database
		WHERE datname = current_database()) LIMIT 1`).Scan(&qid)
	if err != nil {
		t.Skipf("marker not tracked by pg_stat_statements: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
			(queryid, hint_text, symptom) VALUES ($1, 'Set(work_mem "64MB")', 'b28_test')`,
			qid); err != nil {
			t.Fatalf("insert hint: %v", err)
		}
	}
	return qid
}

// b28HintCount runs the promotion check and returns the role's hint_count.
func b28HintCount(
	ctx context.Context, t *testing.T, pool *pgxpool.Pool, role string,
) (any, bool) {
	t.Helper()
	cfg := phase2Config()
	cfg.Analyzer.WorkMemPromotionThreshold = 1
	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	for _, f := range a.checkWorkMemPromotion(ctx) {
		if f.ObjectIdentifier == role {
			return f.Detail["hint_count"], true
		}
	}
	return nil, false
}

// b28MarkerTracked reports whether pg_stat_statements still has the marker.
func b28MarkerTracked(
	ctx context.Context, t *testing.T, pool *pgxpool.Pool, qid int64,
) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_stat_statements WHERE queryid = $1",
		qid).Scan(&n); err != nil {
		t.Fatalf("recheck marker: %v", err)
	}
	return n > 0
}
