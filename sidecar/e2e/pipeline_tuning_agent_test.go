//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/tuning"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Roadmap 2.2 end to end: a real workload, the tuning agent with a
// scripted model, the real optimizer admission (HypoPG), then the real
// executor pipeline. The index runs through the policy gate like any
// other action and carries the agent's prediction into the outcome
// ledger the calibration reads.

type e2eModel struct{ answers []string }

func (m *e2eModel) ChatWithTools(_ context.Context, _ []llm.Message, _ []llm.ToolSpec,
	_ llm.ToolOptions) (llm.ToolResult, error) {
	if len(m.answers) == 0 {
		return llm.ToolResult{Content: `{"proposals":[]}`, Tokens: 10}, nil
	}
	a := m.answers[0]
	m.answers = m.answers[1:]
	return llm.ToolResult{Content: a, Tokens: 10}, nil
}

func tuningWorkload(t *testing.T, pool *pgxpool.Pool) (int64, *collector.Snapshot,
	*collector.Snapshot) {
	t.Helper()
	mustExec(t, pool, `DROP TABLE IF EXISTS tun_e2e;
		CREATE TABLE tun_e2e (id bigint PRIMARY KEY, customer_id bigint, v text);
		INSERT INTO tun_e2e SELECT g, g % 5000, 'x' FROM generate_series(1, 100000) g;
		ANALYZE tun_e2e;`)
	ctx := context.Background()
	q := "SELECT id, v FROM public.tun_e2e WHERE customer_id = $1"
	for i := 0; i < 40; i++ {
		if _, err := pool.Exec(ctx, q, int64(i)); err != nil {
			t.Fatalf("workload: %v", err)
		}
	}
	var qid, calls int64
	if err := pool.QueryRow(ctx, `SELECT queryid, calls FROM pg_stat_statements
		WHERE query LIKE '%public.tun_e2e WHERE customer_id%' ORDER BY calls DESC LIMIT 1`).
		Scan(&qid, &calls); err != nil {
		t.Fatalf("pg_stat_statements: %v", err)
	}
	tbl := []collector.TableStats{{SchemaName: "public", RelName: "tun_e2e",
		NLiveTup: 100000, TableBytes: 8 << 20, Relpersistence: "p"}}
	idx := []collector.IndexStats{{SchemaName: "public", RelName: "tun_e2e",
		IndexRelName: "tun_e2e_pkey", IsUnique: true, IsPrimary: true, IsValid: true,
		IndexDef: "CREATE UNIQUE INDEX tun_e2e_pkey ON public.tun_e2e USING btree (id)"}}
	now := time.Now()
	prev := &collector.Snapshot{CollectedAt: now.Add(-5 * time.Minute), Tables: tbl,
		Indexes: idx, Queries: []collector.QueryStats{{QueryID: qid, Query: q, Calls: 10,
			TotalExecTime: 10}}}
	cur := &collector.Snapshot{CollectedAt: now, Tables: tbl, Indexes: idx,
		Queries: []collector.QueryStats{{QueryID: qid, Query: q, Calls: calls,
			TotalExecTime: 5010}}}
	return qid, prev, cur
}

func e2eTuningAgent(t *testing.T, pool *pgxpool.Pool, model tuning.Model) *tuning.Agent {
	t.Helper()
	logf := func(level, format string, args ...any) {
		t.Logf("[%s] "+format, append([]any{level}, args...)...)
	}
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	cfg := config.DefaultConfig()
	opt := cfg.LLM.Optimizer
	opt.MinSnapshots, opt.MinQueryCalls = 0, 1
	settings := tuning.Settings{Tuning: cfg.Tuning, ConfidenceThreshold: 0.5,
		Memory: cfg.LLM.Optimizer.RejectionMemory, Thresholds: tuning.DefaultThresholds(),
		MaxOutputTokens: 4096, Allowed: map[tuning.ProposalType]bool{
			tuning.ProposeIndexCreate: true, tuning.ProposeStatistics: true}}
	return tuning.New(settings, tuning.Deps{Model: model,
		Indexes: optimizer.New(pool, &opt, version, logf), Facts: facts.NewStore(pool),
		Store: tuning.NewPostgresStore(pool, version, catalogread.Default())}, logf)
}

func TestPipelineTuningAgentProposalRunsThroughTheExecutor(t *testing.T) {
	pool := pipelinePool(t)
	mustExec(t, pool, "CREATE EXTENSION IF NOT EXISTS hypopg")
	qid, prev, cur := tuningWorkload(t, pool)
	agent := e2eTuningAgent(t, pool, &e2eModel{answers: []string{`{"proposals":[{"type":` +
		`"index_create","ddl":"CREATE INDEX CONCURRENTLY tun_e2e_customer_idx ON ` +
		`public.tun_e2e (customer_id)","evidence":["S1"],"expected_change_pct":-50}]}`}})
	out, err := agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	var f analyzer.Finding
	for _, cand := range out.Findings {
		if cand.Category == optimizer.OptimizerCategory {
			f = cand
		}
	}
	if f.RecommendedSQL == "" || f.Detail["what_if_verdict"] != optimizer.WhatIfVerified {
		t.Fatalf("admitted finding = %+v", f)
	}
	cfg := autonomousConfig()
	cfg.Verify.IOCapacity = &config.IOCapacityConfig{ReadWriteMBps: 1000, WALMBps: 1000}
	an, ex := newPipelineExecutor(t, pool, cfg)
	monitor := verify.NewIOMonitor(pool, "e2e", 14*24*time.Hour)
	ctx := context.Background()
	if err := monitor.Sample(ctx); err != nil {
		t.Fatalf("prime IO sampler: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := monitor.Sample(ctx); err != nil {
		t.Fatalf("sample IO rate: %v", err)
	}
	ex.WithIOEvidence(monitor)
	driveFinding(t, pool, an, ex, f)
	act, ok := latestActionFor(t, pool, f.Category, f.ObjectIdentifier)
	if !ok || !isExecutedOutcome(act.Outcome) {
		t.Fatalf("the agent's verified index runs through the executor: %+v %v", act, ok)
	}
	if _, valid, exists := indexAccessMethod(t, pool, "tun_e2e_customer_idx"); !exists || !valid {
		t.Fatal("index tun_e2e_customer_idx was not built")
	}
	var source, method, targets string
	if err := pool.QueryRow(ctx, `SELECT predicted->>'source', prediction_method,
		predicted->>'target_queryids' FROM sage.action_outcome WHERE action_log_id = $1`,
		act.ID).Scan(&source, &method, &targets); err != nil {
		t.Fatalf("outcome ledger: %v", err)
	}
	if source != tuning.PredictionSource || method != verify.MethodHypoPG ||
		targets != fmt.Sprintf("[%d]", qid) {
		t.Fatalf("predicted = source %q method %q targets %s", source, method, targets)
	}
}
