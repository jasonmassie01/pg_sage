package tuning

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The agent end to end on real PostgreSQL: a real workload in
// pg_stat_statements, the real optimizer admission with real HypoPG, the
// real rejection memory and the real stores. Only the model is scripted.

type realWorkload struct {
	schema  string
	queryID int64
	prev    *collector.Snapshot
	cur     *collector.Snapshot
}

func requireHypoPG(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"CREATE EXTENSION IF NOT EXISTS hypopg"); err != nil {
		t.Fatalf("HypoPG is part of the test server image: %v", err)
	}
}

// ordersWorkload creates s.orders with 100k rows and runs a selective
// lookup on customer_id, then reads it back from pg_stat_statements.
func ordersWorkload(t *testing.T, pool *pgxpool.Pool) realWorkload {
	t.Helper()
	ctx := context.Background()
	s := freshSchema(t, pool)
	mustExec(t, pool, "CREATE TABLE "+s+".orders (id bigint PRIMARY KEY, customer_id bigint, "+
		"status text, created_at timestamptz)")
	mustExec(t, pool, "INSERT INTO "+s+".orders SELECT g, g % 5000, "+
		"(ARRAY['open','paid','shipped'])[g % 3 + 1], now() - g * interval '1 minute' "+
		"FROM generate_series(1, 100000) g")
	mustExec(t, pool, "ANALYZE "+s+".orders")
	q := "SELECT id, status FROM " + s + ".orders WHERE customer_id = $1"
	var w realWorkload
	w.schema = s
	// Other packages' tests reset pg_stat_statements on the shared test
	// server: run the workload again until it is read back whole.
	calls, err := int64(0), error(nil)
	for attempt := 0; attempt < 5 && calls < 40; attempt++ {
		runOrdersWorkload(t, pool, q)
		err = pool.QueryRow(ctx, `SELECT queryid, calls FROM pg_stat_statements
			WHERE query LIKE $1 AND dbid = (SELECT oid FROM pg_database
			WHERE datname = current_database()) ORDER BY calls DESC LIMIT 1`,
			"%"+s+".orders WHERE customer_id%").Scan(&w.queryID, &calls)
	}
	if err != nil || calls < 40 {
		t.Fatalf("pg_stat_statements must track the workload: %d calls, %v", calls, err)
	}
	tbl := collector.TableStats{SchemaName: s, RelName: "orders", NLiveTup: 100000,
		TableBytes: 8 << 20, Relpersistence: "p"}
	idx := []collector.IndexStats{{SchemaName: s, RelName: "orders",
		IndexRelName: "orders_pkey", IsUnique: true, IsPrimary: true, IsValid: true,
		IndexDef: "CREATE UNIQUE INDEX orders_pkey ON " + s + ".orders USING btree (id)"}}
	// The interval: all but the first 10 calls, with enough time to be a case.
	w.prev = snapAt(t0, []collector.QueryStats{{QueryID: w.queryID, Query: q, Calls: 10,
		TotalExecTime: 10}}, []collector.TableStats{tbl}, idx)
	w.cur = snapAt(t0.Add(5*time.Minute), []collector.QueryStats{{QueryID: w.queryID,
		Query: q, Calls: calls, TotalExecTime: 10 + 5000}}, []collector.TableStats{tbl}, idx)
	return w
}

func runOrdersWorkload(t *testing.T, pool *pgxpool.Pool, q string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		if _, err := pool.Exec(context.Background(), q, int64(i)); err != nil {
			t.Fatalf("workload: %v", err)
		}
	}
}

func realOptimizer(t *testing.T, pool *pgxpool.Pool) *optimizer.Optimizer {
	t.Helper()
	cfg := config.DefaultConfig().LLM.Optimizer
	cfg.MinSnapshots = 0
	cfg.MinQueryCalls = 1
	return optimizer.New(pool, &cfg, serverVersion(t, pool), noLog)
}

func realAgent(t *testing.T, pool *pgxpool.Pool, model Model,
	opt *optimizer.Optimizer) (*Agent, *logSink) {
	t.Helper()
	logs := &logSink{}
	return New(defaultSettings(), Deps{Model: model, Indexes: opt,
		Facts: facts.NewStore(pool), Store: pgStore(t, pool),
		Now: func() time.Time { return t0 }}, logs.fn), logs
}

func TestAgentDB_VerifiedIndexWithRealHypoPG(t *testing.T) {
	pool := dbPool(t)
	requireHypoPG(t, pool)
	w := ordersWorkload(t, pool)
	model := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		toolCall("explain", fmt.Sprintf(`{"queryid":%d}`, w.queryID)),
		answer(fmt.Sprintf(`{"proposals":[{"type":"index_create","ddl":"CREATE INDEX `+
			`CONCURRENTLY orders_customer_idx ON %s.orders (customer_id)",`+
			`"evidence":["S1","R1"],"expected_change_pct":-50}]}`, w.schema))}}
	agent, logs := realAgent(t, pool, model, realOptimizer(t, pool))
	out, err := agent.Tune(context.Background(), w.cur, w.prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	f, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if !ok {
		t.Fatalf("findings = %+v logs %v", out.Findings, logs.lines)
	}
	if f.Detail["what_if_verdict"] != optimizer.WhatIfVerified {
		t.Fatalf("HypoPG measured the index: %v", f.Detail)
	}
	pe, _ := f.Detail["predicted_effect"].(map[string]any)
	expected, _ := pe["expected_change_pct"].(float64)
	if pe["method"] != verify.MethodHypoPG || expected > -10 {
		t.Fatalf("predicted effect = %v", pe)
	}
	if !strings.Contains(f.RecommendedSQL, "CONCURRENTLY") || f.RollbackSQL == "" {
		t.Fatalf("finding = %+v", f)
	}
	// The explain tool really planned the statement on this server; a
	// parameterized statement has a plan only on PostgreSQL 16+ (generic).
	toolMsg := model.msgs[1][len(model.msgs[1])-1].Content
	if serverVersion(t, pool) >= 160000 && !strings.Contains(toolMsg, "orders") {
		t.Fatalf("explain result = %q", toolMsg)
	}
	if serverVersion(t, pool) < 160000 && !strings.Contains(toolMsg, PlanSourceNone) {
		t.Fatalf("before PG16 the parameterized statement has no plan: %q", toolMsg)
	}
}

func TestAgentDB_RejectedIdeaIsRememberedAndNotRemeasured(t *testing.T) {
	pool := dbPool(t)
	requireHypoPG(t, pool)
	w := ordersWorkload(t, pool)
	useless := fmt.Sprintf(`{"proposals":[{"type":"index_create","ddl":"CREATE INDEX `+
		`CONCURRENTLY orders_created_idx ON %s.orders (created_at)","evidence":["S1"],`+
		`"expected_change_pct":-30},{"type":"create_statistics","table":"%s.orders",`+
		`"columns":["customer_id","status"],"kinds":["dependencies"],"evidence":["S1"],`+
		`"expected_change_pct":-20}]}`, w.schema, w.schema)
	model := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		answer(useless), answer(useless)}}
	opt := realOptimizer(t, pool)
	agent, logs := realAgent(t, pool, model, opt)
	if _, err := agent.Tune(context.Background(), w.cur, w.prev); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if !logs.contains(string(ReasonWhatIfRejected)) {
		t.Fatalf("cycle 1 measures and rejects the index: %v", logs.lines)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM
		sage.optimizer_rejection WHERE schema_name = $1`, w.schema).Scan(&rows); err != nil ||
		rows != 1 {
		t.Fatalf("rejection memory rows = %d %v", rows, err)
	}
	out, err := agent.Tune(context.Background(), w.cur, w.prev)
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if !logs.contains(string(ReasonAlreadyMeasured)) || opt.MemoryStats().WhatIfSkipped != 1 {
		t.Fatalf("cycle 2 does not re-measure: skips %d logs %v",
			opt.MemoryStats().WhatIfSkipped, logs.lines)
	}
	if _, ok := findingByCategory(out.Findings, CategoryStatistics); !ok {
		t.Fatalf("the statistics proposal is still admitted: %+v", out.Findings)
	}
}

func TestAgentDB_ConfirmedFactFromTheRealStore(t *testing.T) {
	pool := dbPool(t)
	requireHypoPG(t, pool)
	w := ordersWorkload(t, pool)
	store := facts.NewStore(pool)
	_, err := store.Declare(context.Background(), facts.Proposal{
		Type: facts.TypeAppMigrations, Kind: facts.KindTable, Subject: w.schema + ".orders",
		Source: facts.SourceOperator, ProposedBy: "test",
		Evidence:  []facts.Citation{{Kind: "operator", Ref: "test", ObservedAt: time.Now()}},
		Rationale: "the app's migrations own it"}, "alice@example.com", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	model := &scriptedModel{turns: []func([]llm.Message) (llm.ToolResult, error){
		answer(fmt.Sprintf(`{"proposals":[{"type":"index_create","ddl":"CREATE INDEX `+
			`CONCURRENTLY orders_customer_idx ON %s.orders (customer_id)",`+
			`"evidence":["S1"],"expected_change_pct":-50}]}`, w.schema))}}
	agent, _ := realAgent(t, pool, model, realOptimizer(t, pool))
	out, err := agent.Tune(context.Background(), w.cur, w.prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	f, ok := findingByCategory(out.Findings, optimizer.OptimizerCategory)
	if !ok || f.RecommendedSQL != "" || f.Detail["source_fix"] == nil {
		t.Fatalf("finding = %+v", f)
	}
	if !strings.Contains(model.msgs[0][1].Content, "operator-confirmed") {
		t.Fatal("the confirmed fact is in the packet")
	}
}
