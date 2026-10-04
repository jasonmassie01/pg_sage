package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Rejection-memory counters are exported per database: what-if evaluations
// the index admission skipped because the idea was already measured, and
// model calls the tuning agent skipped for a case whose recent answers were
// all wasted (the metric names predate the agent and are kept).

func TestWriteOptimizerMemoryMetrics(t *testing.T) {
	var b strings.Builder
	writeOptimizerMemoryMetrics(&b, map[string]analyzer.TuningStats{
		"zeta": {WhatIfSkipped: 4, ModelCallsSkipped: 2, ProposalsCapped: 3,
			TokensUsed: 41000, TokenLimit: 60000, RequestsUsed: 12, RequestLimit: 12,
			CasesAsked: 2, CasesDeferred: 5, DayTokensUsed: 480000,
			DayTokenLimit: 500000},
		"alpha": {},
	})
	out := b.String()
	for _, want := range []string{
		"# HELP pg_sage_optimizer_whatif_skipped_total ",
		"# TYPE pg_sage_optimizer_whatif_skipped_total counter\n",
		"pg_sage_optimizer_whatif_skipped_total{database=\"alpha\"} 0\n",
		"pg_sage_optimizer_whatif_skipped_total{database=\"zeta\"} 4\n",
		"# HELP pg_sage_optimizer_llm_calls_skipped_total ",
		"# TYPE pg_sage_optimizer_llm_calls_skipped_total counter\n",
		"pg_sage_optimizer_llm_calls_skipped_total{database=\"zeta\"} 2\n",
		"# TYPE pg_sage_tuning_proposals_capped_total counter\n",
		"pg_sage_tuning_proposals_capped_total{database=\"zeta\"} 3\n",
		"# TYPE pg_sage_tuning_budget_tokens_used gauge\n",
		"pg_sage_tuning_budget_tokens_used{database=\"zeta\"} 41000\n",
		"pg_sage_tuning_budget_tokens_limit{database=\"zeta\"} 60000\n",
		"pg_sage_tuning_budget_requests_used{database=\"zeta\"} 12\n",
		"pg_sage_tuning_budget_requests_limit{database=\"zeta\"} 12\n",
		"pg_sage_tuning_cases_asked{database=\"zeta\"} 2\n",
		"# TYPE pg_sage_tuning_cases_deferred gauge\n",
		"pg_sage_tuning_cases_deferred{database=\"zeta\"} 5\n",
		"# TYPE pg_sage_tuning_budget_day_tokens_used gauge\n",
		"pg_sage_tuning_budget_day_tokens_used{database=\"zeta\"} 480000\n",
		"pg_sage_tuning_budget_day_tokens_limit{database=\"zeta\"} 500000\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, `{database="alpha"} 0`) > strings.Index(out, `{database="zeta"} 4`) {
		t.Error("databases must be sorted")
	}
}

func TestWriteOptimizerMemoryMetrics_NothingWithoutOptimizers(t *testing.T) {
	var b strings.Builder
	writeOptimizerMemoryMetrics(&b, nil)
	writeOptimizerMemoryFromFleet(&b, nil)
	if b.Len() != 0 {
		t.Fatalf("no optimizer must export nothing, got %q", b.String())
	}
}

// Label values are quoted: a database name cannot break the exposition
// format.
func TestWriteOptimizerMemoryMetrics_QuotesDatabaseNames(t *testing.T) {
	var b strings.Builder
	writeOptimizerMemoryMetrics(&b, map[string]analyzer.TuningStats{`we"ird`: {}})
	if !strings.Contains(b.String(), `{database="we\"ird"} 0`) {
		t.Fatalf("label not escaped:\n%s", b.String())
	}
}
