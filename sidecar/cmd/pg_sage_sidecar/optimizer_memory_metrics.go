package main

import (
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// writeOptimizerMemoryFromFleet exports each database's tuning-agent
// memory counters (databases without an agent are left out). The metric
// names predate the agent and are kept.
func writeOptimizerMemoryFromFleet(b *strings.Builder, mgr *fleet.DatabaseManager) {
	if mgr == nil {
		return
	}
	stats := map[string]analyzer.TuningStats{}
	for name, inst := range mgr.Instances() {
		if inst == nil {
			continue
		}
		if s, ok := inst.Analyzer.TuningStats(); ok {
			stats[name] = s
		}
	}
	writeOptimizerMemoryMetrics(b, stats)
}

// tuningSeries is one per-database series of the tuning agent's stats.
type tuningSeries struct {
	name  string
	value func(analyzer.TuningStats) int64
}

// optimizerMemoryCounters are the per-database rejection-memory series.
var optimizerMemoryCounters = []tuningSeries{
	{"pg_sage_optimizer_whatif_skipped_total",
		func(s analyzer.TuningStats) int64 { return s.WhatIfSkipped }},
	{"pg_sage_optimizer_llm_calls_skipped_total",
		func(s analyzer.TuningStats) int64 { return s.ModelCallsSkipped }},
	{"pg_sage_tuning_proposals_capped_total",
		func(s analyzer.TuningStats) int64 { return s.ProposalsCapped }},
}

// tuningBudgetGauges are the last cycle's budget use and case counts.
var tuningBudgetGauges = []tuningSeries{
	{"pg_sage_tuning_budget_tokens_used",
		func(s analyzer.TuningStats) int64 { return s.TokensUsed }},
	{"pg_sage_tuning_budget_tokens_limit",
		func(s analyzer.TuningStats) int64 { return s.TokenLimit }},
	{"pg_sage_tuning_budget_requests_used",
		func(s analyzer.TuningStats) int64 { return s.RequestsUsed }},
	{"pg_sage_tuning_budget_requests_limit",
		func(s analyzer.TuningStats) int64 { return s.RequestLimit }},
	{"pg_sage_tuning_cases_asked",
		func(s analyzer.TuningStats) int64 { return s.CasesAsked }},
	{"pg_sage_tuning_cases_deferred",
		func(s analyzer.TuningStats) int64 { return s.CasesDeferred }},
	{"pg_sage_tuning_budget_day_tokens_used",
		func(s analyzer.TuningStats) int64 { return s.DayTokensUsed }},
	{"pg_sage_tuning_budget_day_tokens_limit",
		func(s analyzer.TuningStats) int64 { return s.DayTokenLimit }},
}

// writeOptimizerMemoryMetrics writes the counters per database, sorted.
func writeOptimizerMemoryMetrics(b *strings.Builder, stats map[string]analyzer.TuningStats) {
	if len(stats) == 0 {
		return
	}
	dbs := make([]string, 0, len(stats))
	for db := range stats {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	writeTuningSeries(b, optimizerMemoryCounters, dbs, stats)
	writeTuningSeries(b, tuningBudgetGauges, dbs, stats)
	b.WriteString("\n")
}

func writeTuningSeries(b *strings.Builder, series []tuningSeries, dbs []string,
	stats map[string]analyzer.TuningStats) {
	for _, c := range series {
		metricsreg.Header(b, c.name)
		for _, db := range dbs {
			writeSample(b, c.name, dbLabel(db), c.value(stats[db]))
		}
	}
}
