package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/fleet"
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
	name, help string
	value      func(analyzer.TuningStats) int64
}

// optimizerMemoryCounters are the per-database rejection-memory series.
var optimizerMemoryCounters = []tuningSeries{
	{"pg_sage_optimizer_whatif_skipped_total", "HypoPG what-if evaluations index " +
		"admission skipped because the same idea was already measured and rejected",
		func(s analyzer.TuningStats) int64 { return s.WhatIfSkipped }},
	{"pg_sage_optimizer_llm_calls_skipped_total", "Tuning agent model calls skipped " +
		"because the case's recent answers were all wasted and it has not changed",
		func(s analyzer.TuningStats) int64 { return s.ModelCallsSkipped }},
	{"pg_sage_tuning_proposals_capped_total", "Admitted tuning agent proposals the " +
		"per-cycle caps cut before anything was recorded; they wait for a later cycle",
		func(s analyzer.TuningStats) int64 { return s.ProposalsCapped }},
}

// tuningBudgetGauges are the last cycle's budget use and case counts.
var tuningBudgetGauges = []tuningSeries{
	{"pg_sage_tuning_budget_tokens_used", "Model tokens the tuning agent used in its " +
		"last cycle", func(s analyzer.TuningStats) int64 { return s.TokensUsed }},
	{"pg_sage_tuning_budget_tokens_limit", "Model tokens allowed per tuning cycle " +
		"(tuning.max_tokens_per_cycle)",
		func(s analyzer.TuningStats) int64 { return s.TokenLimit }},
	{"pg_sage_tuning_budget_requests_used", "Model requests the tuning agent made in its " +
		"last cycle", func(s analyzer.TuningStats) int64 { return s.RequestsUsed }},
	{"pg_sage_tuning_budget_requests_limit", "Model requests allowed per tuning cycle " +
		"(tuning.max_requests_per_cycle)",
		func(s analyzer.TuningStats) int64 { return s.RequestLimit }},
	{"pg_sage_tuning_cases_asked", "Workload cases the model examined in the last cycle",
		func(s analyzer.TuningStats) int64 { return s.CasesAsked }},
	{"pg_sage_tuning_cases_deferred", "Workload cases the last cycle left for a later " +
		"cycle (case cap, budget, model error); their open findings stay open",
		func(s analyzer.TuningStats) int64 { return s.CasesDeferred }},
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
	writeTuningSeries(b, "counter", optimizerMemoryCounters, dbs, stats)
	writeTuningSeries(b, "gauge", tuningBudgetGauges, dbs, stats)
	b.WriteString("\n")
}

func writeTuningSeries(b *strings.Builder, typ string, series []tuningSeries, dbs []string,
	stats map[string]analyzer.TuningStats) {
	for _, c := range series {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", c.name, c.help, c.name, typ)
		for _, db := range dbs {
			fmt.Fprintf(b, "%s{database=%q} %d\n", c.name, db, c.value(stats[db]))
		}
	}
}
