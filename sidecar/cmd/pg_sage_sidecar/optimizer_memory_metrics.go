package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// writeOptimizerMemoryFromFleet exports each database's optimizer
// rejection-memory counters (databases without an optimizer are left out).
func writeOptimizerMemoryFromFleet(b *strings.Builder, mgr *fleet.DatabaseManager) {
	if mgr == nil {
		return
	}
	stats := map[string]optimizer.MemoryStats{}
	for name, inst := range mgr.Instances() {
		if inst == nil {
			continue
		}
		if s, ok := inst.Analyzer.OptimizerMemoryStats(); ok {
			stats[name] = s
		}
	}
	writeOptimizerMemoryMetrics(b, stats)
}

// optimizerMemoryCounters are the per-database rejection-memory series.
var optimizerMemoryCounters = []struct {
	name, help string
	value      func(optimizer.MemoryStats) int64
}{
	{"pg_sage_optimizer_whatif_skipped_total", "HypoPG what-if evaluations the index " +
		"optimizer skipped because the same idea was already measured and rejected",
		func(s optimizer.MemoryStats) int64 { return s.WhatIfSkipped }},
	{"pg_sage_optimizer_llm_calls_skipped_total", "Index optimizer LLM calls skipped " +
		"because the table's recent proposals were all already measured",
		func(s optimizer.MemoryStats) int64 { return s.LLMCallsSkipped }},
}

// writeOptimizerMemoryMetrics writes the counters per database, sorted.
func writeOptimizerMemoryMetrics(b *strings.Builder, stats map[string]optimizer.MemoryStats) {
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
