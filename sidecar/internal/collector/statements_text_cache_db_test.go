package collector

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// collection-11 on PostgreSQL: the collector reads pg_stat_statements
// texts only when its cache lacks a candidate's, and returns what the old
// single statement did. Each test owns a database: pg_stat_statements is
// cluster-wide, but every read here is scoped to one database, so other
// packages' statements cannot move the counts.

// textTestDB creates a database of its own with pg_stat_statements and a
// small table, and returns a pool on it.
func textTestDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	admin := testPool(t)
	requireStatStatements(t, ctx, New(admin, testConfig(), 0, noopLog))
	name := pgx.Identifier{fmt.Sprintf("pgss_text_%d_%d", os.Getpid(),
		fixtureSeq.Add(1))}.Sanitize()
	mustExec(t, admin, "CREATE DATABASE "+name)
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+
			" WITH (FORCE)")
		if err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.Database = strings.Trim(name, `"`)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return seedTextDB(t, pool)
}

// seedTextDB adds pg_stat_statements and the small table to pool's
// database.
func seedTextDB(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	mustExec(t, pool, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	mustExec(t, pool, "CREATE TABLE text_w (id int PRIMARY KEY, v text)")
	mustExec(t, pool, "INSERT INTO text_w SELECT g, md5(g::text) "+
		"FROM generate_series(1, 2000) g")
	return pool
}

// seedTextWorkload runs five application statements of distinct cost, each
// slower than any of the collector's reads of an idle database.
func seedTextWorkload(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, sql := range []string{
		"SELECT pg_sleep(0.15), count(*) FROM text_w",
		"SELECT pg_sleep(0.12), max(v) FROM text_w",
		"SELECT pg_sleep(0.09), min(id), max(id) FROM text_w",
		"SELECT pg_sleep(0.06), v FROM text_w WHERE id = 7",
		"SELECT pg_sleep(0.03), count(*) FROM text_w a JOIN text_w b USING (id)",
	} {
		mustExec(t, pool, sql)
	}
}

// textReads is how many text reads this database has run.
func textReads(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	view, err := extschema.StatementsRel(ctx, pool, "pg_stat_statements")
	if err != nil {
		t.Fatalf("count text reads: %v", err)
	}
	var n int64
	err = pool.QueryRow(ctx, `SELECT COALESCE(sum(calls), 0)::int8 FROM `+view+`
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND starts_with(query, $1)`, statementTextsTag).Scan(&n)
	if err != nil {
		t.Fatalf("count text reads: %v", err)
	}
	return n
}

// textCycle runs the queries step the way a cycle does: epoch first.
func textCycle(t *testing.T, ctx context.Context, c *Collector) []QueryStats {
	t.Helper()
	c.stmtTexts.observeEpoch(c.collectStatementsEpoch(ctx))
	qs, err := c.collectQueries(ctx)
	if err != nil {
		t.Fatalf("collect queries: %v", err)
	}
	return qs
}

// warmCollector returns a fresh collector after a throwaway one ran every
// statement a cycle runs: those exist in pg_stat_statements from then on.
func warmCollector(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	maxQueries int, full bool) *Collector {
	t.Helper()
	cfg := testConfig()
	cfg.Collector.MaxQueries = maxQueries
	version := serverVersion(t, pool)
	textReads(t, ctx, pool)
	warm := New(pool, cfg, version, noopLog)
	for i := 0; i < 2; i++ {
		if !full {
			textCycle(t, ctx, warm)
		} else if _, err := warm.collect(ctx); err != nil {
			t.Fatalf("warm-up collect: %v", err)
		}
	}
	return New(pool, cfg, version, noopLog)
}

func hasQueryText(qs []QueryStats, sub string) bool {
	for _, q := range qs {
		if strings.Contains(q.Query, sub) {
			return true
		}
	}
	return false
}

// Cycle 1 reads once, cycles 2-3 read nothing, a new query adds one read.
func TestCollectQueries_TextReadsFollowTheCache(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	seedTextWorkload(t, pool)
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		c := warmCollector(t, ctx, pool, 5000, false)
		base := textReads(t, ctx, pool)
		var problems []string
		expect := func(stage string, want int64) {
			if got := textReads(t, ctx, pool) - base; got != want {
				problems = append(problems, fmt.Sprintf("%s: %d text reads, want %d",
					stage, got, want))
			}
		}
		if qs := textCycle(t, ctx, c); !hasQueryText(qs, "max(v) FROM text_w") {
			problems = append(problems, "cycle 1 lacks a seeded statement")
		}
		expect("cycle 1", 1)
		textCycle(t, ctx, c)
		textCycle(t, ctx, c)
		expect("cycles 2-3", 1)
		mustExec(t, pool, "SELECT count(*) FROM text_w WHERE v > 'f' AND id > 10")
		if qs := textCycle(t, ctx, c); !hasQueryText(qs, "v > $1 AND id > $2") {
			problems = append(problems, "the new statement is missing after cycle 4")
		}
		expect("a new statement", 2)
		return problems
	})
}

// After pg_stat_statements_reset() the next cycle reads the texts again.
func TestCollectQueries_ResetReadsTextsAgain(t *testing.T) {
	ctx := context.Background()
	pool := seedTextDB(t, exclusivePool(t, "the pg_stat_statements stats_reset epoch"))
	seedTextWorkload(t, pool)
	c := warmCollector(t, ctx, pool, 5000, false)
	rec := &sqlRecorder{}
	c.onCatalogQuery = rec.hook
	textCycle(t, ctx, c)
	if n := rec.count(statementTextsTag); n != 1 {
		t.Fatalf("%d text reads in cycle 1, want 1", n)
	}
	mustExec(t, pool, "SELECT pg_stat_statements_reset()")
	mustExec(t, pool, "SELECT pg_sleep(0.01), count(*) FROM text_w WHERE id < 50")
	qs := textCycle(t, ctx, c)
	if n := rec.count(statementTextsTag); n != 2 {
		t.Fatalf("%d text reads after the reset, want 2", n)
	}
	if !hasQueryText(qs, "WHERE id < $") {
		t.Fatalf("queries after the reset lack the statement run since: %d", len(qs))
	}
}

// CHECK-01: ten steady collection cycles read pg_stat_statements texts at
// most once.
func TestCollect_TenSteadyCyclesReadTextsAtMostOnce(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	seedTextWorkload(t, pool)
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		c := warmCollector(t, ctx, pool, 500, true)
		rec := &sqlRecorder{}
		c.onCatalogQuery = rec.hook
		base := textReads(t, ctx, pool)
		for i := 0; i < 10; i++ {
			if _, err := c.collect(ctx); err != nil {
				return []string{fmt.Sprintf("collect %d: %v", i, err)}
			}
		}
		pgss := textReads(t, ctx, pool) - base
		hook := rec.count(`pg_stat_statements"(true)`) +
			rec.count("count(*) FILTER (WHERE kw NOT IN") // the classification
		if pgss > 1 || hook > 1 {
			return []string{fmt.Sprintf("10 cycles: %d text reads in pg_stat_statements, "+
				"%d text reads sent; want at most 1", pgss, hook)}
		}
		return nil
	})
}

// CHECK-02: the split read returns the old single statement's queries, in
// its order, with its counters (golden), with room to spare and when the
// limit cuts. The collector's pool is tagged as in production: its own
// statements (transaction control included), which run between the two
// reads, are left out by both, so only the workload is compared. Since
// collection-07 the mean and io arms may admit up to 2K rows the total
// ranking did not: the golden covers the rows the total ranking admitted.
func TestCollectQueries_MatchesLegacySingleRead(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	seedTextWorkload(t, pool)
	sage := taggedPool(t, ctx, pool)
	for _, limit := range []int{5000, 3} {
		pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
			c := warmCollector(t, ctx, sage, limit, false)
			want, err := legacyQueries(ctx, c)
			if err != nil {
				return []string{fmt.Sprintf("legacy read: %v", err)}
			}
			got := textCycle(t, ctx, c)
			if limit == 3 && len(want) != 3 {
				return []string{fmt.Sprintf("legacy read gave %d queries, want 3", len(want))}
			}
			byTotal, extra := splitAdmitted(got)
			if problems := extraAdmissions(limit, extra, want); problems != nil {
				return problems
			}
			return diffQueries(limit, byTotal, want[:min(len(want), len(byTotal))])
		})
	}
}

// splitAdmitted separates the queries the total ranking admitted from those
// only the mean, io or pinned arms did (collection-07).
func splitAdmitted(qs []QueryStats) (byTotal, extra []QueryStats) {
	for _, q := range qs {
		if slices.Contains(q.AdmittedBy, "total") {
			byTotal = append(byTotal, q)
		} else {
			extra = append(extra, q)
		}
	}
	return byTotal, extra
}

// extraAdmissions checks the rows only collection-07's arms admitted: at
// most 2K of them, none in the old read's top-N (which they would have
// been admitted by), and room left for the total ranking.
func extraAdmissions(limit int, extra, want []QueryStats) []string {
	if len(extra) > 2*admissionFor(limit).perArm {
		return []string{fmt.Sprintf("limit %d: %d rows beyond the total ranking", limit,
			len(extra))}
	}
	for _, q := range extra {
		for _, w := range want {
			if w.QueryID == q.QueryID {
				return []string{fmt.Sprintf("limit %d: query %d is in the top-N by total "+
					"but admitted only by %v", limit, q.QueryID, q.AdmittedBy)}
			}
		}
	}
	return nil
}

// taggedPool is a pool on pool's database configured as pg_sage's own
// (selfmonitor.ConfigurePool): every statement it sends carries the tag.
func taggedPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	selfmonitor.ConfigurePool(cfg)
	tagged, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect a tagged pool: %v", err)
	}
	t.Cleanup(tagged.Close)
	return tagged
}

// legacyQueries runs the pre-collection-11 single statement.
func legacyQueries(ctx context.Context, c *Collector) ([]QueryStats, error) {
	caps := c.queryCapabilities(ctx)
	sql := fmt.Sprintf(legacyQueryStatsTemplate(caps.WALColumns, caps.PlanTimeColumns),
		caps.blk.read, caps.blk.write, c.maxQueries())
	// The golden text names the view bare; a pinned session (security-01c)
	// resolves it only through its schema.
	view, err := extschema.StatementsRel(ctx, c.pool, "pg_stat_statements")
	if err != nil {
		return nil, err
	}
	sql = strings.ReplaceAll(sql, " pg_stat_statements(", " "+view+"(")
	rows, err := c.catalogQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueryStats
	for rows.Next() {
		var q QueryStats
		dest := queryCounterDest(&q, caps.WALColumns, caps.PlanTimeColumns)
		dest = append([]any{dest[0], &q.Query}, dest[1:]...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// diffQueries lists where got differs from want: ids, order, text, counters.
func diffQueries(limit int, got, want []QueryStats) []string {
	if len(got) != len(want) || len(want) == 0 {
		return []string{fmt.Sprintf("limit %d: %d queries, legacy %d (want equal, > 0)",
			limit, len(got), len(want))}
	}
	var problems []string
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }
	for i := range want {
		g, w := got[i], want[i]
		ints := g.QueryID == w.QueryID && g.Query == w.Query && g.Calls == w.Calls &&
			g.Rows == w.Rows && g.SharedBlksHit == w.SharedBlksHit &&
			g.SharedBlksRead == w.SharedBlksRead && g.TempBlksWritten == w.TempBlksWritten &&
			g.WALRecords == w.WALRecords && g.WALBytes == w.WALBytes
		floats := near(g.TotalExecTime, w.TotalExecTime) &&
			near(g.MeanExecTime, w.MeanExecTime) && near(g.StddevExecTime, w.StddevExecTime) &&
			near(g.MaxExecTime, w.MaxExecTime) && near(g.TotalPlanTime, w.TotalPlanTime) &&
			near(g.BlkReadTime, w.BlkReadTime)
		if !ints || !floats {
			problems = append(problems, fmt.Sprintf("limit %d, row %d:\n got %+v\nwant %+v",
				limit, i, g, w))
		}
	}
	return problems
}

// Concurrent access: LatestSnapshot readers run against collection cycles
// and only ever see whole snapshots without pg_sage's own statements.
func TestLatestSnapshot_ReadersDuringCycles(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	seedTextWorkload(t, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	done := make(chan struct{})
	bad := make(chan string, 4)
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if s := c.LatestSnapshot(); s != nil && (s.Queries == nil ||
					hasQueryText(s.Queries, "pg_sage")) {
					bad <- fmt.Sprintf("snapshot queries %v", idsOf(s.Queries))
					return
				}
			}
		}()
	}
	for i := 0; i < 3; i++ {
		c.cycle(ctx, &fakeTicker{})
	}
	close(done)
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Fatal(msg)
	}
	if s := c.LatestSnapshot(); s == nil || !hasQueryText(s.Queries, "max(v) FROM text_w") ||
		c.PreviousSnapshot() == nil {
		t.Fatalf("after 3 cycles latest = %v, previous = %v, want both with the workload",
			s, c.PreviousSnapshot())
	}
}
