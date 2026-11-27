package tuning

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// tuning-08: the agent's plan is bounded by age, ANALYZE and pg_sage
// actions; a stale cache is replaced by one plain EXPLAIN or refused.

func seedPlanTable(t *testing.T, s string) {
	t.Helper()
	pool := dbPool(t)
	mustExec(t, pool, "CREATE TABLE "+s+".t (id bigint PRIMARY KEY, v int)")
	mustExec(t, pool, "INSERT INTO "+s+".t SELECT g, g FROM generate_series(1, 1000) g")
	mustExec(t, pool, "ANALYZE "+s+".t")
}

func cachePlanAgo(t *testing.T, qid int64, ago, plan string) {
	t.Helper()
	pool := dbPool(t)
	mustExec(t, pool, `INSERT INTO sage.explain_cache (queryid, captured_at, plan_json, source)
		VALUES ($1, now() - $2::interval, $3, 'auto_explain')`, qid, ago, plan)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_cache WHERE queryid=$1",
			qid)
	})
}

func TestPostgresStore_FreshCachedPlanCarriesCapturedAt(t *testing.T) {
	pool := dbPool(t)
	s := freshSchema(t, pool)
	seedPlanTable(t, s)
	qid := -time.Now().UnixNano()
	cachePlanAgo(t, qid, "0 seconds", `[{"Plan":{"Node Type":"Index Scan",`+
		`"Relation Name":"t","Schema":"`+s+`"}}]`)
	p, err := pgStore(t, pool).Plan(context.Background(), qid, "SELECT 1")
	if err != nil || p.Source != PlanSourceCache || p.Capture != "auto_explain" ||
		p.CapturedAt.IsZero() || !strings.Contains(string(p.JSON), "Index Scan") {
		t.Fatalf("fresh cached plan = %+v, %v", p, err)
	}
}

func TestPostgresStore_StalePlanWithFailingExplainIsRefused(t *testing.T) {
	pool := dbPool(t)
	qid := -time.Now().UnixNano()
	cachePlanAgo(t, qid, "30 hours", `[{"Plan":{"Node Type":"Index Scan"}}]`)
	p, err := pgStore(t, pool).Plan(context.Background(), qid,
		"SELECT * FROM no_such_table_tuning08")
	if err == nil || !strings.Contains(err.Error(), "no_such_table_tuning08") ||
		len(p.JSON) != 0 {
		t.Fatalf("a failing fresh EXPLAIN must be refused, never the stale plan: %+v, %v",
			p, err)
	}
}

func TestPostgresStore_StaleCacheParameterizedStatement(t *testing.T) {
	pool := dbPool(t)
	s := freshSchema(t, pool)
	seedPlanTable(t, s)
	qid := -time.Now().UnixNano()
	cachePlanAgo(t, qid, "30 hours", `[{"Plan":{"Node Type":"Index Scan"}}]`)
	p, err := pgStore(t, pool).Plan(context.Background(), qid,
		"SELECT * FROM "+s+".t WHERE v = $1")
	if strings.Contains(string(p.JSON), "Index Scan") {
		t.Fatalf("the stale cached plan was returned: %+v", p)
	}
	if serverVersion(t, pool) < 160000 {
		if err != nil || p.Source != PlanSourceNone {
			t.Fatalf("before PG16 a parameterized statement has no plan: %+v %v", p, err)
		}
		return
	}
	if err != nil || p.Source != PlanSourceGeneric || p.HasActuals ||
		!strings.Contains(string(p.JSON), "Seq Scan") {
		t.Fatalf("PG16+ plans it with GENERIC_PLAN: %+v %v", p, err)
	}
}

func TestPostgresStore_PlanMaxAgeOption(t *testing.T) {
	pool := dbPool(t)
	qid := -time.Now().UnixNano()
	cachePlanAgo(t, qid, "5 minutes", `[{"Plan":{"Node Type":"Index Scan"}}]`)
	short := NewPostgresStore(pool, serverVersion(t, pool), catalogread.Default(),
		PlanMaxAge(time.Minute))
	p, err := short.Plan(context.Background(), qid, "SELECT 1")
	if err != nil || p.Source != PlanSourceExplain || !strings.Contains(string(p.JSON),
		"Result") {
		t.Fatalf("a 5-minute plan over a 1-minute limit is replaced by EXPLAIN: %+v %v", p, err)
	}
	p, err = pgStore(t, pool).Plan(context.Background(), qid, "SELECT 1")
	if err != nil || p.Source != PlanSourceCache {
		t.Fatalf("under the 24h default it is still fresh: %+v %v", p, err)
	}
}
