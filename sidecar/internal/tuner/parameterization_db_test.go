package tuner

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// tuning-11 on real PostgreSQL: 25 statements whose IN lists have 1..25
// constants are 25 queryids before PG18, so one tuner cycle reports one
// query_parameterization finding; PG18 squashes the lists into one queryid
// and the trigger is off. The cases projector turns the finding into a
// prepare_parameterized_query script candidate.

const paramDBTable = "tuning11_param_orders"

// pgssSource is the collector's view of pg_stat_statements for this
// database: what the latest snapshot's Queries hold.
type pgssSource struct {
	t    *testing.T
	pool *pgxpool.Pool
}

func (s pgssSource) LatestSnapshot() *collector.Snapshot {
	rows, err := s.pool.Query(context.Background(), `SELECT queryid, query, calls,
		total_exec_time, mean_exec_time FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND queryid IS NOT NULL`)
	if err != nil {
		s.t.Fatalf("read pg_stat_statements: %v", err)
	}
	defer rows.Close()
	snap := &collector.Snapshot{}
	for rows.Next() {
		var q collector.QueryStats
		if err := rows.Scan(&q.QueryID, &q.Query, &q.Calls, &q.TotalExecTime,
			&q.MeanExecTime); err != nil {
			s.t.Fatalf("scan pg_stat_statements: %v", err)
		}
		snap.Queries = append(snap.Queries, q)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatalf("pg_stat_statements rows: %v", err)
	}
	return snap
}

func runListVariants(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{
		"DROP TABLE IF EXISTS " + paramDBTable,
		"CREATE TABLE " + paramDBTable + " (id int PRIMARY KEY, total numeric)",
		"INSERT INTO " + paramDBTable + " SELECT g, g FROM generate_series(1, 100) g",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if _, err := testdb.ResetStatements(ctx, pool); err != nil {
		t.Fatalf("reset this database's pg_stat_statements: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+paramDBTable)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.findings WHERE category = 'query_parameterization'`)
	})
	for i := 1; i <= n; i++ {
		lits := make([]string, i)
		for p := range lits {
			lits[p] = fmt.Sprint(p + 1)
		}
		sql := "SELECT id, total FROM " + paramDBTable + " WHERE id IN (" +
			strings.Join(lits, ", ") + ")"
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("run variant %d: %v", i, err)
		}
	}
}

func serverVersionNum(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}
	return v
}

func TestParameterization_DB_ListVariantsOneTunerCycle(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE category = 'query_parameterization'`)
	runListVariants(t, pool, 25)
	pgNum := serverVersionNum(t, pool)
	tu := New(pool, TunerConfig{MinQueryCalls: 100, PlanTimeRatio: 3, WorkMemMaxMB: 256},
		nil, noopLogFn)
	tu.WithQuerySource(pgssSource{t: t, pool: pool}, pgNum)
	fs, err := tu.Tune(ctx, nil)
	if err != nil {
		t.Fatalf("Tune: %v", err)
	}
	var mine []analyzer.Finding
	for _, f := range paramFindings(fs) {
		if q, _ := f.Detail["normalized_query"].(string); strings.Contains(q, paramDBTable) {
			mine = append(mine, f)
		}
	}
	if pgNum >= 180000 {
		if len(mine) != 0 {
			t.Fatalf("PG18 squashes lists, yet %d findings: %+v", len(mine), mine)
		}
		return
	}
	if len(mine) != 1 {
		t.Fatalf("findings = %d, want 1 on PG %d: %+v", len(mine), pgNum, mine)
	}
	f := mine[0]
	want := "SELECT id, total FROM " + paramDBTable + " WHERE id IN (...)"
	if f.Detail["normalized_query"] != want || f.Detail["variant_count"] != 25 ||
		f.Detail["pg_major"] != pgNum/10000 {
		t.Fatalf("detail = %v, want %q, 25 variants, pg_major %d", f.Detail, want, pgNum/10000)
	}
	assertParamPersisted(t, pool, mine)
	assertParamCandidate(t, f)
}

// CHECK-26: one tuner cycle leaves one query_parameterization row.
func assertParamPersisted(t *testing.T, pool *pgxpool.Pool, fs []analyzer.Finding) {
	t.Helper()
	ctx := context.Background()
	if err := analyzer.UpsertFindings(ctx, pool, fs); err != nil {
		t.Fatalf("upsert findings: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE category = 'query_parameterization'`).Scan(&n); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if n != 1 {
		t.Fatalf("sage.findings query_parameterization rows = %d, want 1", n)
	}
}

func assertParamCandidate(t *testing.T, f analyzer.Finding) {
	t.Helper()
	c := cases.ProjectFinding(cases.SourceFinding{ID: "41", Category: f.Category,
		Severity: cases.Severity(f.Severity), ObjectType: f.ObjectType,
		ObjectIdentifier: f.ObjectIdentifier, Title: f.Title,
		Recommendation: f.Recommendation, Detail: f.Detail}, nil)
	if len(c.ActionCandidates) != 1 {
		t.Fatalf("candidates = %+v, want one", c.ActionCandidates)
	}
	a := c.ActionCandidates[0]
	if a.ActionType != "prepare_parameterized_query" || a.RiskTier != "moderate" {
		t.Fatalf("candidate = %s/%s, want prepare_parameterized_query/moderate",
			a.ActionType, a.RiskTier)
	}
	if a.ScriptOutput == nil || !strings.Contains(a.ScriptOutput.MigrationSQL,
		"WHERE id IN (...)") {
		t.Fatalf("script output = %+v, want the collapsed shape", a.ScriptOutput)
	}
}
