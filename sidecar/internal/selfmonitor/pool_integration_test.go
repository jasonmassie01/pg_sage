package selfmonitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// selfPool opens a pool configured exactly as pg_sage's own pools are.
func selfPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var loaded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname = 'pg_stat_statements')`).Scan(&loaded); err != nil || !loaded {
		t.Skipf("pg_stat_statements not installed: %v", err)
	}
	return pool, ctx
}

// pgssTexts returns this database's pg_stat_statements texts containing
// marker (an identifier: literals are normalized away).
func pgssTexts(t *testing.T, ctx context.Context, marker string) []string {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(ctx, `SELECT query FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND strpos(query, $1) > 0`, marker)
	if err != nil {
		t.Skipf("pg_stat_statements unreadable (not preloaded?): %v", err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	return texts
}

// Product decision (perf v1.8.3): pg_sage no longer hides from
// pg_stat_statements. Every statement it sends, over every protocol path
// pgx uses, reaches the server with the /* pg_sage */ tag in front, so a
// DBA sees its cost and pg_sage's own analysis can leave it out.
func TestConfigurePool_EveryProtocolPathIsTaggedAndTracked(t *testing.T) {
	pool, ctx := selfPool(t)
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.selftag_probe
		(id int, v text)`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.selftag_probe")
	})
	var n int
	// Extended protocol (prepared, cached statement).
	if err := pool.QueryRow(ctx, "SELECT $1::int AS selftag_ext", 7).Scan(&n); err != nil || n != 7 {
		t.Fatalf("extended query: n=%d err=%v", n, err)
	}
	// Simple protocol.
	// (Each probe has its own shape: pg_stat_statements ignores aliases.)
	if _, err := pool.Exec(ctx, "SELECT 8, 9 AS selftag_simple",
		pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("simple query: %v", err)
	}
	// Batch (pipelined Parse/Bind/Execute).
	batch := &pgx.Batch{}
	batch.Queue("SELECT 1, 2, 3 AS selftag_batch_a")
	batch.Queue("SELECT 1, 2, 3, 4 AS selftag_batch_b")
	if err := pool.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatalf("batch: %v", err)
	}
	// COPY FROM STDIN.
	copied, err := pool.CopyFrom(ctx, pgx.Identifier{"public", "selftag_probe"},
		[]string{"id", "v"}, pgx.CopyFromRows([][]any{{1, "a"}, {2, "b"}}))
	if err != nil || copied != 2 {
		t.Fatalf("copy: copied=%d err=%v", copied, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) AS selftag_count FROM public.selftag_probe").
		Scan(&n); err != nil || n != 2 {
		t.Fatalf("COPY rows not readable back: n=%d err=%v", n, err)
	}
	for _, marker := range []string{"selftag_ext", "selftag_simple", "selftag_batch_a",
		"selftag_batch_b", "selftag_count"} {
		texts := pgssTexts(t, ctx, marker)
		if len(texts) == 0 {
			t.Errorf("%s: not tracked by pg_stat_statements (pg_sage must stay visible)", marker)
		}
		for _, text := range texts {
			if !IsTagged(text) {
				t.Errorf("%s tracked untagged: %q", marker, text)
			}
		}
	}
}

func TestConfigurePool_SessionIsNamedAndNotUntracked(t *testing.T) {
	pool, ctx := selfPool(t)
	var app, track string
	err := pool.QueryRow(ctx, `SELECT current_setting('application_name'),
		COALESCE(current_setting('pg_stat_statements.track', true), 'unset')`).Scan(&app, &track)
	if err != nil {
		t.Fatalf("read session settings: %v", err)
	}
	if app != ApplicationName {
		t.Errorf("application_name = %q, want %q", app, ApplicationName)
	}
	if track == "none" {
		t.Error("pg_stat_statements.track = none: pg_sage is hiding its own cost")
	}
}

// The exclusion predicates pg_sage's readers share recognize its own
// statements and sessions and keep everyone else's.
func TestExclusionPredicatesEvaluateInPostgres(t *testing.T) {
	pool, ctx := selfPool(t)
	statements := map[string]bool{ // text -> kept (not pg_sage)
		"SELECT /* pg_sage */ 1":                    false,
		"/* pg_sage */ SELECT 1":                    false,
		"/* pg_sage sre:lock_graph v1 */ SELECT 1":  false,
		"SELECT * FROM sage.findings":               false,
		`SELECT * FROM "sage".decision`:             false,
		"SELECT * FROM public.orders WHERE id = $1": true,
		"SELECT * FROM message.sage_notes":          true,
		"":                                          true,
	}
	for text, kept := range statements {
		var got bool
		sql := "SELECT " + StatementExclusionSQL("$1::text")
		if err := pool.QueryRow(ctx, sql, text).Scan(&got); err != nil {
			t.Fatalf("evaluate statement exclusion: %v", err)
		}
		if got != kept {
			t.Errorf("statement %q kept = %v, want %v", text, got, kept)
		}
	}
	apps := map[string]bool{"pg_sage": false, "pg_sage_fleet": false, "psql": true, "": true}
	for app, kept := range apps {
		var got bool
		sql := "SELECT " + strings.Replace(ActivityExclusionSQL(""),
			"application_name", "$1::text", 1)
		if err := pool.QueryRow(ctx, sql, app).Scan(&got); err != nil {
			t.Fatalf("evaluate activity exclusion: %v", err)
		}
		if got != kept {
			t.Errorf("application %q kept = %v, want %v", app, got, kept)
		}
	}
}
