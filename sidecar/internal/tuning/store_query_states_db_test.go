package tuning

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// tuning-06 integration: QueryStates reads pg_stat_statements and
// sage.query_hints for the open query_tuning findings, and a statement
// removed from pg_stat_statements resolves its finding after N cycles.

func requirePGSS(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (
		SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')`).Scan(&ok); err != nil {
		t.Fatalf("read pg_extension: %v", err)
	}
	if !ok {
		t.Skip("pg_stat_statements is not installed on the test server")
	}
}

// runStatement runs sql n times and returns the queryid of the
// pg_stat_statements entry whose (normalized) text contains fragment.
func runStatement(t *testing.T, pool *pgxpool.Pool, sql, fragment string, n int) int64 {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		mustExec(t, pool, sql)
	}
	var qid int64
	if err := pool.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements
		WHERE strpos(query, $1) > 0 AND query NOT LIKE '%pg_stat_statements%'
		  AND dbid = (SELECT oid FROM pg_database
		                             WHERE datname = current_database())
		LIMIT 1`, fragment).Scan(&qid); err != nil {
		t.Fatalf("queryid of %q: %v", sql, err)
	}
	return qid
}

func TestPostgresStore_QueryStates(t *testing.T) {
	pool := dbPool(t)
	requirePGSS(t, pool)
	s := freshSchema(t, pool)
	mustExec(t, pool, "CREATE TABLE "+s+".qs (a int)")
	fast := runStatement(t, pool, "SELECT count(*) FROM "+s+".qs", "count(*) FROM "+s+".qs", 5)
	slow := runStatement(t, pool, "SELECT pg_sleep(0.11), max(a) FROM "+s+".qs",
		"max(a) FROM "+s+".qs", 2)
	const missing = int64(-77_000_001)
	mustExec(t, pool, "DELETE FROM sage.query_hints WHERE queryid = ANY($1)",
		[]int64{fast, slow, missing})
	t.Cleanup(func() {
		mustExec(t, pool, "DELETE FROM sage.query_hints WHERE queryid = ANY($1)",
			[]int64{fast, slow, missing})
	})
	mustExec(t, pool, `INSERT INTO sage.query_hints (queryid, hint_text, symptom, status)
		VALUES ($1, 'SeqScan(qs)', 's', 'rejected'), ($1, 'SeqScan(qs)', 's', 'proposed'),
		       ($2, 'SeqScan(qs)', 's', 'proposed'), ($2, 'SeqScan(qs)', 's', 'expired')`,
		fast, slow)
	st, err := pgStore(t, pool).QueryStates(context.Background(),
		[]int64{fast, slow, missing}, HintAdmission{MinCalls: 2, PlanTimeRatio: 3})
	if err != nil {
		t.Fatalf("QueryStates: %v", err)
	}
	if got := st[fast]; !got.Present || got.Admitted || got.HintStatus != "proposed" {
		t.Fatalf("fast statement = %+v, want present, not admitted, latest proposed", got)
	}
	if got := st[slow]; !got.Present || !got.Admitted || got.HintStatus != "expired" {
		t.Fatalf("slow statement = %+v, want present, admitted, latest expired", got)
	}
	if got, ok := st[missing]; !ok || got.Present || got.Admitted || got.HintStatus != "" {
		t.Fatalf("missing statement = %+v (reported %t), want reported absent", got, ok)
	}
	high, err := pgStore(t, pool).QueryStates(context.Background(), []int64{slow},
		HintAdmission{MinCalls: 3, PlanTimeRatio: 3})
	if err != nil || high[slow].Admitted {
		t.Fatalf("2 calls under min_query_calls 3 = %+v, %v; want not admitted", high, err)
	}
	if empty, err := pgStore(t, pool).QueryStates(context.Background(), nil,
		HintAdmission{}); err != nil || len(empty) != 0 {
		t.Fatalf("no queryids = %v, %v; want nothing read", empty, err)
	}
}

// A statement reset out of pg_stat_statements resolves its open finding
// after N cycles, not before; the statement left in place stays open.
func TestQueryHint_ResetQueryidResolvesAfterNCycles(t *testing.T) {
	pool := dbPool(t)
	requirePGSS(t, pool)
	s := freshSchema(t, pool)
	mustExec(t, pool, "CREATE TABLE "+s+".rq (a int)")
	gone := runStatement(t, pool, "SELECT count(*) FROM "+s+".rq", "count(*) FROM "+s+".rq", 3)
	kept := runStatement(t, pool, "SELECT max(a) FROM "+s+".rq", "max(a) FROM "+s+".rq", 3)
	mustExec(t, pool, "SELECT pg_stat_statements_reset(0, 0, $1)", gone)
	h := newHarness(t)
	h.agent.deps.Store = pgStore(t, pool)
	h.agent.settings.HintAdmission = HintAdmission{MinCalls: 1, PlanTimeRatio: 3}
	open := []analyzer.Finding{queryHintFinding(gone), queryHintFinding(kept)}
	outs := keepCycles(h, open, 3, nil, nil)
	goneID := open[0].ObjectIdentifier
	for i := 0; i < 2; i++ {
		if _, ok := byIdent(outs[i], goneID); !ok {
			t.Fatalf("cycle %d: the reset statement resolved before N=3", i+1)
		}
	}
	if _, ok := byIdent(outs[2], goneID); ok {
		t.Fatal("the reset statement is open after 3 absent cycles")
	}
	if _, ok := byIdent(outs[2], open[1].ObjectIdentifier); !ok {
		t.Fatal("the statement still in pg_stat_statements resolved")
	}
}
