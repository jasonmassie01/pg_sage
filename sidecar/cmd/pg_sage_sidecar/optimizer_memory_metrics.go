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

// optimizerMemoryCounters are the per-database rejection-memory series.
var optimizerMemoryCounters = []struct {
	name, help string
	value      func(analyzer.TuningStats) int64
}{
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
	for _, c := range optimizerMemoryCounters {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
		for _, db := range dbs {
			fmt.Fprintf(b, "%s{database=%q} %d\n", c.name, db, c.value(stats[db]))
		}
	}
	b.WriteString("\n")
}
