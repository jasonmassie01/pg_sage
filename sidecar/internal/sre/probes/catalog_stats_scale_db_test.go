package probes

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The pg_stat view probes at scale (PR #116 CI: stat_statements hit its
// statement timeout on a server with ~50000 pg_stat_statements entries).
// The views must stay well under the probe timeout however much text
// pg_stat_statements holds and however many tables the database has:
// stat_statements never loads query text, stat_tables formats names for
// its top rows only.

// bestOf runs a probe three times and returns the fastest usable run
// (transient load on a shared server is not the probe's cost).
func bestOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id ID) Result {
	t.Helper()
	var best Result
	for i := 0; i < 3; i++ {
		res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, id, Args{})
		if !res.Status.Usable() {
			t.Fatalf("%s run %d = %+v", id, i+1, res)
		}
		if i == 0 || res.ElapsedMS < best.ElapsedMS {
			best = res
		}
	}
	return best
}

// fillStatements records n distinct statements with long texts in
// pg_stat_statements (track = all, so the DO block's statements count).
func fillStatements(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET pg_stat_statements.track = 'all'"); err != nil {
		t.Fatalf("track all statements (the test needs a superuser): %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET pg_stat_statements.track") }()
	if _, err := conn.Exec(ctx, fmt.Sprintf(`DO $$
DECLARE k int; m int; q text; i int; done int := 0;
BEGIN
  FOR k IN 1..100 LOOP
    FOR m IN 1..100 LOOP
      EXIT WHEN done >= %d;
      q := 'SELECT v.id';
      FOR i IN 1..m LOOP q := q || ', v.id * ' || i || ' + ' || i || ' AS sre_scale_' || i; END LOOP;
      q := q || ' FROM (VALUES (1)) v(id) WHERE v.id = 0';
      FOR i IN 1..k LOOP q := q || ' OR v.id = ' || i; END LOOP;
      EXECUTE q;
      done := done + 1;
    END LOOP;
  END LOOP;
END $$`, n)); err != nil {
		t.Fatalf("fill pg_stat_statements: %v", err)
	}
}

func TestCatalog_StatStatementsStaysFastWithManyLongStatements(t *testing.T) {
	pool, ctx := statPool(t)
	fillStatements(t, ctx, pool, 3000)
	res := bestOf(t, ctx, pool, StatStatements)
	if limit := MaxStatementTimeout / 2; time.Duration(res.ElapsedMS)*time.Millisecond >=
		limit {
		t.Fatalf("stat_statements took %d ms with thousands of long statements, want "+
			"under %s (half its timeout)", res.ElapsedMS, limit)
	}
	if len(res.Rows) == 0 || len(res.Rows) > statViewRows {
		t.Fatalf("stat_statements returned %d rows, want 1-%d", len(res.Rows), statViewRows)
	}
	prev := -1.0
	for i, row := range res.Rows {
		if _, ok := row["query"]; ok {
			t.Fatalf("row %d carries query text: %+v", i, row)
		}
		if _, ok := row["own_role"].(bool); !ok {
			t.Fatalf("row %d lacks own_role: %+v", i, row)
		}
		ms, _ := row["total_exec_ms"].(float64)
		if prev >= 0 && ms > prev {
			t.Fatalf("rows are not ordered by total_exec_ms: %v after %v", ms, prev)
		}
		prev = ms
	}
}

func TestCatalog_StatTablesStaysFastWithManyTables(t *testing.T) {
	pool, ctx := statPool(t)
	schema := fmt.Sprintf("sre_scale_%d", time.Now().UnixNano()%1_000_000_000)
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
	})
	if _, err := pool.Exec(ctx, `DO $$ BEGIN FOR i IN 1..1500 LOOP
		EXECUTE format('CREATE TABLE %I.%I (id int)', '`+schema+`', 't' || i);
	END LOOP; END $$`); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	res := bestOf(t, ctx, pool, StatTables)
	if limit := MaxStatementTimeout / 2; time.Duration(res.ElapsedMS)*time.Millisecond >=
		limit {
		t.Fatalf("stat_tables took %d ms over 1500+ tables, want under %s", res.ElapsedMS,
			limit)
	}
	if len(res.Rows) == 0 || len(res.Rows) > statViewRows {
		t.Fatalf("stat_tables returned %d rows, want 1-%d", len(res.Rows), statViewRows)
	}
	for _, row := range res.Rows {
		if rel, _ := row["relation"].(string); rel == "" || strings.HasPrefix(rel, "sage.") {
			t.Fatalf("stat_tables row %+v: want a named user table outside sage", row)
		}
	}
}

func TestCatalog_StatStatementsNeverLoadsQueryText(t *testing.T) {
	sql := strings.Join(strings.Fields(statStatementsSQL), " ")
	if !strings.Contains(sql, "pg_stat_statements(showtext => false)") ||
		regexp.MustCompile(`\.query`).MatchString(sql) || strings.Contains(sql, "(true)") {
		t.Fatalf("stat_statements must read pg_stat_statements without text:\n%s", sql)
	}
	if !slices.Contains(strings.Fields(sql), "LIMIT") {
		t.Fatalf("stat_statements must be bounded:\n%s", sql)
	}
}
