package collector

import (
	"context"
	"fmt"
	"io"
	"testing"
)

// pg_stat_statements composition (dogfood lifeos, 2026-10-04: 4,859 of
// 5,000 entries, 3,701 of them pg_dump "COPY ... TO stdout" statements,
// 100 deallocations): the collector reads how full the statistics are and
// how much of it is utility statements, so the analyzer can say why.

func TestNearStatementsCapacity_Boundary(t *testing.T) {
	cases := []struct {
		entries, max int
		want         bool
	}{
		{3999, 5000, false},
		{4000, 5000, true},
		{5000, 5000, true},
		{0, 5000, false},
		{10, 0, false},
		{-1, 5000, false},
	}
	for _, c := range cases {
		if got := nearStatementsCapacity(c.entries, c.max); got != c.want {
			t.Errorf("nearStatementsCapacity(%d, %d) = %v, want %v", c.entries, c.max, got,
				c.want)
		}
	}
}

func requireStatStatements(t *testing.T, ctx context.Context, c *Collector) {
	t.Helper()
	var ok bool
	if err := c.pool.QueryRow(ctx, "SELECT to_regclass('pg_stat_statements') IS NOT NULL").
		Scan(&ok); err != nil {
		t.Fatalf("check pg_stat_statements: %v", err)
	}
	if !ok {
		t.Skip("pg_stat_statements is not installed on this server")
	}
}

func TestReadStatStatementsUsage_CountsUtilityAndCopyOut(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	requireStatStatements(t, ctx, c)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	for i := 0; i < 3; i++ { // three distinct pg_dump-style COPY ... TO STDOUT
		sql := fmt.Sprintf("COPY (SELECT %d AS usage_probe_%d) TO STDOUT", i, i)
		if _, err := conn.Conn().PgConn().CopyTo(ctx, io.Discard, sql); err != nil {
			conn.Release()
			t.Fatalf("copy out: %v", err)
		}
	}
	conn.Release()
	u, err := c.readStatStatementsUsage(ctx, true)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if u.Entries < 3 || u.CopyOut < 3 || u.Utility < u.CopyOut || u.Utility > u.Entries {
		t.Fatalf("usage = %+v, want >= 3 COPY TO STDOUT counted as utility", u)
	}
	if !u.Classified || u.TrackUtility != "on" {
		t.Fatalf("usage = %+v, want classified with track_utility on (the default)", u)
	}
	if u.Dealloc < 0 {
		t.Fatalf("dealloc = %d, want it read from pg_stat_statements_info", u.Dealloc)
	}
	cheap, err := c.readStatStatementsUsage(ctx, false)
	if err != nil {
		t.Fatalf("read unclassified usage: %v", err)
	}
	if cheap.Classified || cheap.Utility != 0 || cheap.CopyOut != 0 || cheap.Entries < 3 {
		t.Fatalf("unclassified usage = %+v, want only the entry count", cheap)
	}
}

// Without the extension's settings the collector records nothing rather
// than zeros that would read as "empty".
func TestCollectStatStatementsUsage_NoMaxIsNil(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	if u := c.collectStatStatementsUsage(context.Background(), 0); u != nil {
		t.Fatalf("usage without pg_stat_statements.max = %+v, want nil", u)
	}
}

// The classification itself, on fixed texts (pg_stat_statements is
// cluster-wide, so counts read from it move with other sessions).
func TestStatementsClassification_FixedTexts(t *testing.T) {
	pool := testPool(t)
	src := `(VALUES ('COPY public.t (a, b) TO stdout;'),
		('  copy (SELECT 1) to STDOUT'),
		('COPY t FROM STDIN'),
		('/* pg_sage */ SELECT 1'),
		('(SELECT 1) UNION (SELECT 2)'),
		('WITH x AS (SELECT 1) SELECT * FROM x'),
		('SET work_mem = ''64MB'''),
		('VACUUM t'),
		('INSERT INTO t VALUES (1)'),
		('<insufficient privilege>')) AS v(query)`
	var entries, utility, copyOut int
	if err := pool.QueryRow(context.Background(), classifySQL(src)).
		Scan(&entries, &utility, &copyOut); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if entries != 10 || utility != 5 || copyOut != 2 {
		t.Fatalf("entries %d utility %d copy_out %d, want 10, 5 (3 COPY, SET, VACUUM), 2",
			entries, utility, copyOut)
	}
}
