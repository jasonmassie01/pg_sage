package collector

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// pg_stat_statements texts are read only where they are returned
// (coordinator audit, 2026-10-04): on a 45k-entry, 14 MB-text server the
// queries read sorted every entry with its text on disk (external merge,
// 16 MB) and ran the self-exclusion regex on all of them, 265-325 ms of
// the 500 ms budget; ranking on the counters first and matching texts of
// the 1,000 candidates only took 156-178 ms.

type sqlRecorder struct {
	mu  sync.Mutex
	sql []string
}

func (r *sqlRecorder) hook(_ context.Context, _ pgx.Tx, sql string, _ []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sql = append(r.sql, sql)
}

func (r *sqlRecorder) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.sql {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// The counters are ranked without any text, and the text read asks for the
// given ids of this database only (collection-11).
func TestQueryCountersSQL_ReadsNoText(t *testing.T) {
	for _, wal := range []bool{false, true} {
		for _, plan := range []bool{false, true} {
			sql := queryStatsTemplate(wal, plan)
			// %[6]s: the qualified pg_stat_statements function (security-01c-q).
			if !strings.Contains(sql, "FROM %[6]s(false)") ||
				strings.Contains(sql, "%[6]s(true)") ||
				strings.Contains(sql, "array_agg(query") ||
				!strings.Contains(sql, "LIMIT %d") || candidateHeadroom != 2 {
				t.Errorf("wal %v plan %v: want a text-free ranking of 2N:\n%s",
					wal, plan, sql)
			}
		}
	}
	texts := statementTextsSQL(testPgss.fn)
	for _, want := range []string{`"ext"."pg_stat_statements"(true)`, "queryid = ANY($1)",
		"datname = current_database()", "array_agg(query ORDER BY calls DESC)"} {
		if !strings.Contains(texts, want) {
			t.Errorf("text read lacks %q:\n%s", want, texts)
		}
	}
	if !strings.HasPrefix(texts, statementTextsTag) {
		t.Errorf("text read does not start with its tag %q", statementTextsTag)
	}
}

// The ranked read still leaves pg_sage's own statements out and returns
// application statements.
func TestCollectQueries_RankedReadExcludesSelf(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	requireStatStatements(t, ctx, c)
	if _, err := pool.Exec(ctx, "SELECT 4242 AS ranked_app_marker"); err != nil {
		t.Fatalf("app statement: %v", err)
	}
	if _, err := pool.Exec(ctx, sageTag+"SELECT 4343 AS ranked_self_marker"); err != nil {
		t.Fatalf("self statement: %v", err)
	}
	c.cfg.Collector.MaxQueries = 5000
	qs, err := c.collectQueries(ctx)
	if err != nil {
		t.Fatalf("collect queries: %v", err)
	}
	var app, self bool
	for _, q := range qs {
		app = app || strings.Contains(q.Query, "ranked_app_marker")
		self = self || strings.Contains(q.Query, "ranked_self_marker")
	}
	if !app || self {
		t.Fatalf("app statement found %v (want true), pg_sage's %v (want false) in %d",
			app, self, len(qs))
	}
}

// Near capacity, the texts are classified at most every classifyEvery;
// the entry count is fresh every cycle.
func TestCollectStatStatementsUsage_ClassifiesOnSlowCadence(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	requireStatStatements(t, ctx, c)
	clock := time.Unix(6_000_000, 0)
	c.now = func() time.Time { return clock }
	rec := &sqlRecorder{}
	c.onCatalogQuery = rec.hook
	const classify = "FILTER (WHERE kw = 'copy'"
	first := c.collectStatStatementsUsage(ctx, 1) // 1 entry max: near capacity
	if first == nil || !first.Classified || rec.count(classify) != 1 {
		t.Fatalf("first read = %+v after %d classifications, want one", first,
			rec.count(classify))
	}
	clock = clock.Add(classifyEvery - time.Second)
	again := c.collectStatStatementsUsage(ctx, 1)
	if again == nil || !again.Classified || rec.count(classify) != 1 ||
		again.Utility != first.Utility || again.CopyOut != first.CopyOut {
		t.Fatalf("read inside the interval = %+v after %d classifications, want the "+
			"cached counts", again, rec.count(classify))
	}
	if again.Entries < 1 {
		t.Fatalf("entries = %d, want a fresh count", again.Entries)
	}
	clock = clock.Add(2 * time.Second)
	bumpStatementEntries(t, ctx, c) // collection-11: an unchanged count is not reread
	if u := c.collectStatStatementsUsage(ctx, 1); u == nil || rec.count(classify) != 2 {
		t.Fatalf("read after the interval: %+v, %d classifications, want 2", u,
			rec.count(classify))
	}
	far := c.collectStatStatementsUsage(ctx, 1<<30) // far from capacity
	if far == nil || far.Classified || rec.count(classify) != 2 {
		t.Fatalf("far from capacity = %+v after %d classifications, want unclassified",
			far, rec.count(classify))
	}
}

// bumpStatementEntries adds a pg_stat_statements entry no earlier run made:
// a statement on a new temporary table (its oid is part of the queryid).
func bumpStatementEntries(t *testing.T, ctx context.Context, c *Collector) {
	t.Helper()
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	name := fmt.Sprintf("classify_bump_%d_%d", os.Getpid(), fixtureSeq.Add(1))
	for _, sql := range []string{"CREATE TEMP TABLE " + name + " (a int)",
		"SELECT a FROM " + name, "DROP TABLE " + name} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}
