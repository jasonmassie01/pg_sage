package explain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// tuning-10: query_id resolves the pg_stat_statements text of the current
// database, prefers a fresh captured plan with actuals, and otherwise
// explains the text afresh. Real PostgreSQL with pg_stat_statements.

type qidFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	schema string
}

func newQIDFixture(t *testing.T) *qidFixture {
	t.Helper()
	pool := liveExplainPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	requirePGSSOn(t, ctx, pool)
	f := &qidFixture{t: t, ctx: ctx, pool: pool, schema: randomName("qid_")}
	f.exec("CREATE SCHEMA " + f.schema)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+f.schema+" CASCADE")
		if err != nil {
			t.Errorf("drop schema %s: %v", f.schema, err)
		}
	})
	return f
}

func requirePGSSOn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	if err == nil {
		_, err = pool.Exec(ctx, "SELECT 1 FROM pg_stat_statements LIMIT 1")
	}
	if err != nil {
		t.Skipf("pg_stat_statements unavailable on the test server: %v", err)
	}
}

func randomName(prefix string) string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}

func (f *qidFixture) exec(stmts ...string) {
	f.t.Helper()
	for _, s := range stmts {
		if _, err := f.pool.Exec(f.ctx, s); err != nil {
			f.t.Fatalf("%s: %v", s, err)
		}
	}
}

// table creates <schema>.<prefix><random> with rows rows of 32-character
// payloads. It is never analyzed (autovacuum off too), so a capture taken
// before the test's call stays fresh: an ANALYZE would make it stale.
func (f *qidFixture) table(prefix string, rows int) string {
	f.t.Helper()
	q := f.schema + "." + randomName(prefix)
	f.exec(fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY, customer_id int NOT NULL, "+
		"payload text NOT NULL) WITH (autovacuum_enabled = false)", q),
		fmt.Sprintf("INSERT INTO %s SELECT i, i %% 100, md5(i::text) "+
			"FROM generate_series(1, %d) i", q, rows))
	return q
}

// statementID returns the queryid of db's pg_stat_statements entry whose
// text contains fragment.
func statementIDOn(t *testing.T, ctx context.Context, db *pgxpool.Pool,
	fragment string) (int64, bool) {
	t.Helper()
	var id int64
	err := db.QueryRow(ctx, `SELECT s.queryid FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid
		WHERE d.datname = current_database() AND strpos(s.query, $1) > 0
		  AND strpos(s.query, 'pg_stat_statements') = 0
		ORDER BY s.calls DESC LIMIT 1`, fragment).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("look up statement %q: %v", fragment, err)
	}
	return id, true
}

// capture stores a plan for id in sage.explain_cache, captured age ago.
func (f *qidFixture) capture(id int64, plan string, analyzed bool, age time.Duration) {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx, `INSERT INTO sage.explain_cache
		(queryid, query_text, plan_json, source, captured_at, analyze_enabled)
		VALUES ($1, 'captured', $2::jsonb, 'auto_explain', now() - $3 * interval '1 second', $4)`,
		id, plan, age.Seconds(), analyzed)
	if err != nil {
		f.t.Fatalf("store capture: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			"DELETE FROM sage.explain_cache WHERE queryid = $1", id)
	})
}

// spillCapture is an executed plan of rel whose sort spilled to disk.
func spillCapture(rel string) string {
	return `[{"Plan": {"Node Type": "Sort", "Total Cost": 500, "Plan Rows": 1000,
		"Actual Rows": 1000, "Actual Total Time": 30, "Actual Loops": 1,
		"Sort Method": "external merge", "Sort Space Used": 8396, "Sort Space Type": "Disk",
		"Plans": [{"Node Type": "Seq Scan", "Relation Name": "` + rel + `",
			"Total Cost": 100, "Plan Rows": 1000, "Actual Rows": 1000,
			"Actual Total Time": 5, "Actual Loops": 1}]},
		"Execution Time": 41.5}]`
}

// paramStatement runs a $1 statement on rel and returns its queryid.
func (f *qidFixture) paramStatement(rel string) (int64, string) {
	f.t.Helper()
	sql := "SELECT id FROM " + rel + " WHERE customer_id = $1"
	for i := 0; i < 3; i++ {
		if _, err := f.pool.Exec(f.ctx, sql, 7); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	}
	id, ok := statementIDOn(f.t, f.ctx, f.pool, rel+" WHERE customer_id = $1")
	if !ok {
		return 0, ""
	}
	return id, sql
}

func TestQueryID_ExplainsStatementOfThisDatabase(t *testing.T) {
	f := newQIDFixture(t)
	rel := f.table("orders_", 2000)
	ex := New(f.pool, liveConfig(10000), noopLogFn)
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		id, sql := f.paramStatement(rel)
		if id == 0 {
			return []string{"statement missing from pg_stat_statements"}
		}
		res, err := ex.Explain(f.ctx, ExplainRequest{QueryID: QueryID(id)})
		if err != nil {
			t.Fatalf("Explain(query_id=%d): %v", id, err)
		}
		if res.Query != sql {
			t.Errorf("Query = %q, want the pg_stat_statements text %q", res.Query, sql)
		}
		if res.PlanSource != "explain" || res.PlanAgeS == nil || *res.PlanAgeS != 0 {
			t.Errorf("plan_source=%q plan_age_s=%v, want a fresh explain of age 0",
				res.PlanSource, res.PlanAgeS)
		}
		if res.SlowBecause == nil || res.Insights == nil || len(res.NodeBreakdown) == 0 {
			t.Errorf("result lacks the rules floor or nodes: %+v", res)
		}
		if strings.Contains(res.Summary, "LLM analysis pending") {
			t.Errorf("Summary = %q, want a rules summary", res.Summary)
		}
		return nil
	})
}

func TestQueryID_OtherDatabaseIsInvalid(t *testing.T) {
	f := newQIDFixture(t)
	other, err := pgxpool.New(f.ctx, testdb.CreateDatabase(t, "explain_qid_other"))
	if err != nil {
		t.Fatalf("connect other database: %v", err)
	}
	t.Cleanup(other.Close)
	requirePGSSOn(t, f.ctx, other)
	rel := randomName("elsewhere_")
	if _, err := other.Exec(f.ctx, "CREATE TABLE "+rel+" (id int)"); err != nil {
		t.Fatalf("create table in other database: %v", err)
	}
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		for i := 0; i < 3; i++ {
			if _, err := other.Exec(f.ctx, "SELECT id FROM "+rel+" WHERE id = $1", i); err != nil {
				t.Fatalf("run statement in other database: %v", err)
			}
		}
		id, ok := statementIDOn(t, f.ctx, other, rel)
		if !ok {
			return []string{"statement missing from the other database's pg_stat_statements"}
		}
		_, err := New(f.pool, liveConfig(10000), noopLogFn).
			Explain(f.ctx, ExplainRequest{QueryID: QueryID(id)})
		if !errors.Is(err, ErrExplainInvalidRequest) || !errors.Is(err, ErrUnknownQueryID) {
			t.Fatalf("err = %v, want an invalid request for an unknown query id", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprint(id)) {
			t.Errorf("err = %v, want it to name the id %d", err, id)
		}
		return nil
	})
}

func TestQueryID_PrefersCapturedPlanWithActuals(t *testing.T) {
	f := newQIDFixture(t)
	rel := f.table("captured_", 2000)
	ex := New(f.pool, liveConfig(10000), noopLogFn)
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		id, sql := f.paramStatement(rel)
		if id == 0 {
			return []string{"statement missing from pg_stat_statements"}
		}
		f.capture(id, spillCapture(strings.TrimPrefix(rel, f.schema+".")), true, 90*time.Second)
		res, err := ex.Explain(f.ctx, ExplainRequest{QueryID: QueryID(id)})
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if res.PlanSource != "explain_cache" || res.PlanCapture != "auto_explain" {
			t.Fatalf("plan_source=%q capture=%q, want the auto_explain capture",
				res.PlanSource, res.PlanCapture)
		}
		// The capture time is the server's clock and the age the sidecar's,
		// so allow a few seconds of clock skew between them.
		if res.PlanAgeS == nil || *res.PlanAgeS < 80 || *res.PlanAgeS > 150 {
			t.Errorf("plan_age_s = %s, want about 90", fmtAge(res.PlanAgeS))
		}
		if res.Query != sql || res.ActualTimeMs == nil || *res.ActualTimeMs != 41.5 {
			t.Errorf("query=%q actual=%v, want the statement and the capture's 41.5 ms",
				res.Query, res.ActualTimeMs)
		}
		if len(res.SlowBecause) == 0 || !strings.Contains(res.SlowBecause[0], "8.2 MB") {
			t.Errorf("SlowBecause = %v, want the capture's spill", res.SlowBecause)
		}
		return nil
	})
}

func TestQueryID_CaptureWithoutActualsOrStaleIsReplaced(t *testing.T) {
	cases := []struct {
		name     string
		analyzed bool
		age      time.Duration
	}{
		{"estimates only", false, 30 * time.Second},
		{"older than the age limit", true, 25 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newQIDFixture(t)
			rel := f.table("replaced_", 500)
			ex := New(f.pool, liveConfig(10000), noopLogFn)
			pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
				id, _ := f.paramStatement(rel)
				if id == 0 {
					return []string{"statement missing from pg_stat_statements"}
				}
				f.capture(id, spillCapture(strings.TrimPrefix(rel, f.schema+".")),
					c.analyzed, c.age)
				res, err := ex.Explain(f.ctx, ExplainRequest{QueryID: QueryID(id)})
				if err != nil {
					t.Fatalf("Explain: %v", err)
				}
				if res.PlanSource != "explain" || strings.Contains(string(res.PlanJSON), "8396") {
					t.Errorf("plan_source=%q, want a fresh EXPLAIN instead of the capture",
						res.PlanSource)
				}
				return nil
			})
		})
	}
}

// CHECK-22 at the package boundary the REST handler calls: with no LLM,
// a statement named by query_id whose executed plan spilled its sort gets
// a non-empty slow_because. The executed plan is a real EXPLAIN ANALYZE
// this test runs under a small work_mem and stores as auto_explain would
// (this build cannot ANALYZE through the explainer: no cgo parser).
func TestQueryID_SpillingStatementHasRulesWithoutLLM(t *testing.T) {
	f := newQIDFixture(t)
	rel := f.table("spill_", 100000)
	ex := New(f.pool, liveConfig(30000), noopLogFn)
	sql := "SELECT payload FROM " + rel + " ORDER BY payload"
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		f.exec(sql)
		id, ok := statementIDOn(t, f.ctx, f.pool, rel+" ORDER BY payload")
		if !ok {
			return []string{"statement missing from pg_stat_statements"}
		}
		f.capture(id, f.analyzedPlan(sql), true, 0)
		res, err := ex.Explain(f.ctx, ExplainRequest{QueryID: QueryID(id)})
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		found := false
		for _, s := range res.SlowBecause {
			found = found || strings.Contains(s, "sort spilled")
		}
		if !found || res.PlanSource != "explain_cache" {
			t.Errorf("SlowBecause = %v (source %q), want the sort spill", res.SlowBecause,
				res.PlanSource)
		}
		return nil
	})
}

// analyzedPlan runs EXPLAIN ANALYZE of sql with work_mem 64kB on one
// connection and returns the JSON plan.
func (f *qidFixture) analyzedPlan(sql string) string {
	f.t.Helper()
	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		f.t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(f.ctx, "SET work_mem = '64kB'"); err != nil {
		f.t.Fatalf("set work_mem: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET work_mem") }()
	var plan string
	if err := conn.QueryRow(f.ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql).Scan(&plan); err != nil {
		f.t.Fatalf("explain analyze: %v", err)
	}
	if !strings.Contains(plan, `"Disk"`) {
		f.t.Fatalf("the sort did not spill at work_mem 64kB: %s", plan)
	}
	return plan
}

// Concurrent access: two Explain calls on one Explainer.
func TestExplain_ConcurrentCallsOnOneExplainer(t *testing.T) {
	f := newQIDFixture(t)
	rel := f.table("concurrent_", 1000)
	ex := New(f.pool, liveConfig(10000), noopLogFn)
	queries := []string{"SELECT count(*) FROM " + rel,
		"SELECT max(id) FROM " + rel + " WHERE customer_id = 3"}
	var wg sync.WaitGroup
	results := make([]*ExplainResult, len(queries))
	errs := make([]error, len(queries))
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			results[i], errs[i] = ex.Explain(f.ctx, ExplainRequest{Query: q})
		}(i, q)
	}
	wg.Wait()
	for i, q := range queries {
		if errs[i] != nil {
			t.Fatalf("Explain(%q): %v", q, errs[i])
		}
		if results[i].Query != q || results[i].PlanSource != "explain" ||
			len(results[i].NodeBreakdown) == 0 {
			t.Errorf("result %d = %q (source %q), want its own planned query %q",
				i, results[i].Query, results[i].PlanSource, q)
		}
	}
}

// fmtAge prints an optional age for a failure message.
func fmtAge(age *int64) string {
	if age == nil {
		return "nil"
	}
	return fmt.Sprint(*age)
}
