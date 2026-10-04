package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The investigator's safe EXPLAIN (roadmap 2.1): the model names a
// statement by its pg_stat_statements queryid, never by SQL. Only a
// single read statement is explained, plan-only (no ANALYZE ever, so the
// statement never runs), in a read-only transaction under the probe
// timeouts; the evidence carries plan node shapes, never query text.
// A normalized statement ($n parameters) gets its generic plan, never a
// plan for constant NULLs (the plan must still read the table).

func explainTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("sre_explain_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := pool.Exec(ctx, "CREATE TABLE "+name+
		" (id int PRIMARY KEY, v text)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+name) })
	if _, err := pool.Exec(ctx, "INSERT INTO "+name+
		" SELECT g, 'v' || g FROM generate_series(1, 50) g"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return name
}

// statementID runs sql once and finds its queryid in pg_stat_statements.
func statementID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql, marker string,
	args ...any) int64 {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("run %q: %v", sql, err)
	}
	var id int64
	err := pool.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements
		WHERE query LIKE $1 AND dbid = (SELECT oid FROM pg_database
		WHERE datname = current_database()) ORDER BY calls DESC LIMIT 1`,
		"%"+marker+"%").Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pg_stat_statements does not hold %q (reset or not preloaded)", marker)
	}
	if err != nil {
		t.Fatalf("queryid: %v", err)
	}
	return id
}

func TestStatementExplainer_ExplainsAReadStatementPlanOnly(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE v LIKE 'v1%'",
		"FROM "+tbl+" WHERE v LIKE")
	res := NewStatementExplainer(pool).ExplainStatement(ctx, id)
	if res.Status != probes.StatusOK || res.ProbeID != ExplainProbeID || len(res.Rows) == 0 {
		t.Fatalf("explain = %+v", res)
	}
	found := false
	for _, row := range res.Rows {
		for _, k := range []string{"actual_rows", "actual_total_time", "query", "filter"} {
			if _, ok := row[k]; ok {
				t.Fatalf("plan row carries %q: %+v (ANALYZE or text leaked)", k, row)
			}
		}
		found = found || row["relation"] == tbl
	}
	if !found {
		t.Fatalf("no plan node reads %s: %+v", tbl, res.Rows)
	}
}

func TestStatementExplainer_RefusesWritesAndNeverRunsThem(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "DELETE FROM "+tbl+" WHERE id = 999999",
		"DELETE FROM "+tbl)
	res := NewStatementExplainer(pool).ExplainStatement(ctx, id)
	if res.Status != probes.StatusUnsupported || res.Reason != ReasonNotReadStatement ||
		len(res.Rows) != 0 {
		t.Fatalf("explain of a DELETE = %+v, want unsupported %s", res,
			ReasonNotReadStatement)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil || n != 50 {
		t.Fatalf("rows after = %d (%v), want 50", n, err)
	}
}

func TestStatementExplainer_UnknownAndInvalidQueryIDs(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	ex := NewStatementExplainer(pool)
	if res := ex.ExplainStatement(ctx, 0); res.Status != probes.StatusError ||
		res.Reason != "invalid_args" {
		t.Fatalf("queryid 0 = %+v", res)
	}
	res := ex.ExplainStatement(ctx, 7_777_777_777_777)
	if res.Status != probes.StatusEmpty || res.Reason != ReasonUnknownStatement {
		t.Fatalf("unknown queryid = %+v", res)
	}
	var nilEx *StatementExplainer
	if res := nilEx.ExplainStatement(ctx, 1); res.Status != probes.StatusError {
		t.Fatalf("nil explainer = %+v", res)
	}
}

func TestStatementExplainer_IsBoundedByTheProbeTimeout(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	ex := NewStatementExplainer(pool)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	res := ex.ExplainStatement(cctx, 42)
	if res.Status != probes.StatusError || !strings.Contains(res.Reason, "cancel") {
		t.Fatalf("canceled explain = %+v", res)
	}
}

func TestInvestigator_ExplainToolStoresPlanEvidence(t *testing.T) {
	st, pool, ctx := liveStore(t, budgetLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE id = $1",
		"FROM "+tbl+" WHERE id", 7)
	m := newFakeModel(t,
		call(ToolExplain, fmt.Sprintf(`{"queryid":%d}`, id)),
		submitFixed(invFinal{Outcome: "inconclusive"}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
		invOptions{config: InvestigatorConfig{Explainer: NewStatementExplainer(pool)}})
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "operator:explain", Kind: TriggerOperator,
		Subject: "slow lookup"})
	steps := stepsOf(transcriptOf(t, inv), ToolExplain)
	if len(steps) != 1 || steps[0].EvidenceID == "" || steps[0].Status != "ok" {
		t.Fatalf("explain steps = %+v", steps)
	}
	ev := evidenceByID(t, st, inv)[steps[0].EvidenceID]
	if ev.ProbeID != string(ExplainProbeID) || strings.Contains(string(ev.Payload), "SELECT") {
		t.Fatalf("explain evidence = %s %s", ev.ProbeID, ev.Payload)
	}
}
