package autoexplain

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// guard-02c: a top query whose newest plan predates an ANALYZE of a
// relation it reads is captured again on the next cycle, so a plan flip
// after a statistics refresh is seen within one capture interval instead
// of the next day. No concurrent access tests: candidateSQL is one read.

const nudgeQueryBase = 919_000_000

type nudgeFixture struct {
	pool  *pgxpool.Pool
	table string
}

func newNudgeFixture(t *testing.T) *nudgeFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SAGE_DATABASE_URL"))
	if dsn == "" || strings.Contains(dsn, "pgsage_test_disabled") {
		t.Skip("SKIPPED: SAGE_DATABASE_URL fixture is not configured")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1 // the temporary pg_stat_statements lives on one session
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	bootstrapSageSchema(t, pool)
	f := &nudgeFixture{pool: pool, table: fmt.Sprintf("nudge_%d", time.Now().UnixNano())}
	clean := func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM sage.explain_cache WHERE queryid BETWEEN $1
			AND $1 + 9`, nudgeQueryBase)
		_, _ = pool.Exec(c, "DROP TABLE IF EXISTS public."+f.table)
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(ctx, `CREATE TABLE public.`+f.table+` AS
		SELECT g AS id FROM generate_series(1, 100) g;
		CREATE TEMP TABLE pg_stat_statements (queryid bigint, query text,
		mean_exec_time double precision, calls bigint, dbid oid)`); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	for i := int64(1); i <= 3; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO pg_stat_statements SELECT $1, $2, 500,
			20, oid FROM pg_database WHERE datname = current_database()`,
			nudgeQueryBase+i,
			fmt.Sprintf("SELECT id FROM %s WHERE id = %d", f.table, i)); err != nil {
			t.Fatalf("seed statements: %v", err)
		}
	}
	return f
}

// store caches a plan of the query that reads the fixture table.
func (f *nudgeFixture) store(t *testing.T, qid int64) {
	t.Helper()
	plan := fmt.Sprintf(`[{"Plan": {"Node Type": "Seq Scan", "Relation Name": %q}}]`,
		f.table)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO sage.explain_cache
		(captured_at, queryid, plan_json, source) VALUES (clock_timestamp(), $1, $2::jsonb,
		'explain_estimate')`, qid, plan); err != nil {
		t.Fatalf("store plan: %v", err)
	}
}

// analyze runs ANALYZE and waits until the statistics show it.
func (f *nudgeFixture) analyze(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, "ANALYZE public."+f.table); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var at *time.Time
		err := f.pool.QueryRow(ctx, `SELECT pg_stat_clear_snapshot(), last_analyze
			FROM pg_stat_user_tables WHERE relid = to_regclass($1)`, "public."+f.table).
			Scan(nil, &at)
		if err == nil && at != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("last_analyze of %s never showed (%v)", f.table, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (f *nudgeFixture) candidates(t *testing.T) map[int64]bool {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), candidateSQL("pg_temp.pg_stat_statements"),
		float64(100), 50)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	defer rows.Close()
	got := map[int64]bool{}
	for rows.Next() {
		var id int64
		var q string
		if err := rows.Scan(&id, &q); err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPlanFlip_AnalyzeAfterTheNewestPlanNudgesACapture(t *testing.T) {
	f := newNudgeFixture(t)
	before, after, never := int64(nudgeQueryBase+1), int64(nudgeQueryBase+2),
		int64(nudgeQueryBase+3)
	f.store(t, before)
	f.analyze(t)
	f.store(t, after)
	got := f.candidates(t)
	if !got[before] {
		t.Errorf("query %d: its plan predates the ANALYZE but it is not recaptured", before)
	}
	if got[after] {
		t.Errorf("query %d: its plan follows the ANALYZE but it is recaptured", after)
	}
	if !got[never] {
		t.Errorf("query %d without a plan is no longer a candidate", never)
	}
	// Once recaptured, the nudge stops.
	f.store(t, before)
	if f.candidates(t)[before] {
		t.Errorf("query %d is still a candidate after its recapture", before)
	}
}
