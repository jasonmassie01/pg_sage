package analyzer

import (
	"context"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// TuningProducer is the case-driven tuning agent (roadmap 2.2): one per
// database, it examines the workload's cases and returns typed, gated
// proposals as findings.
type TuningProducer interface {
	Tune(ctx context.Context, current, previous *collector.Snapshot) (TuningOutput, error)
	Stats() TuningStats
}

// TuningOutput is one tuning cycle: the findings, the categories it fully
// judged (their open findings may resolve), the categories it could not
// judge (theirs never resolve this cycle) and the tables it proposes
// indexes for (the query tuner defers them).
type TuningOutput struct {
	Findings    []Finding
	Evaluated   []string
	Failed      []string
	IndexTables []string
}

// TuningStats are the agent's counters since start.
type TuningStats struct {
	WhatIfSkipped     int64 // what-ifs rejection memory skipped (already measured)
	ModelCallsSkipped int64 // cases not sent to the model (wasted before, unchanged)
	ProposalsCapped   int64 // admitted proposals the per-cycle caps cut (never recorded)

	// The last cycle's model budget use and the cases it left for later.
	TokensUsed    int64
	TokenLimit    int64
	RequestsUsed  int64
	RequestLimit  int64
	CasesAsked    int64 // cases the model examined
	CasesDeferred int64 // cases left for a later cycle (case cap, budget, model error)
}

// TuningCategories are the finding categories the tuning agent owns: the
// index categories (with the labels releases before v1.8.0 stored), index
// drops, extended statistics and the configuration it tunes.
func TuningCategories() []string {
	return append(optimizer.Categories(), "tuning_index_drop", "query_create_statistics",
		"memory_tuning", "vacuum_tuning", "table_tuning")
}

// TuningStats returns the tuning agent's counters; ok is false when this
// database runs no agent.
func (a *Analyzer) TuningStats() (TuningStats, bool) {
	if a == nil || a.tuning == nil {
		return TuningStats{}, false
	}
	return a.tuning.Stats(), true
}

// runTuning runs the agent and records what it judged. Without the index
// list the agent would see tables as unindexed and propose indexes that
// exist (dogfood lifeos-1), so it is skipped and its categories stay.
func (a *Analyzer) runTuning(ctx context.Context, current, previous *collector.Snapshot,
) TuningOutput {
	if a.tuning == nil {
		return TuningOutput{}
	}
	if !current.Available("indexes") {
		a.logFn("WARN", "analyzer: tuning agent skipped: indexes unavailable this cycle")
		a.eval.fail(TuningCategories()...)
		return TuningOutput{}
	}
	out, err := a.tuning.Tune(ctx, current, previous)
	a.eval.fail(out.Failed...)
	if err != nil {
		a.logFn("WARN", "analyzer: tuning agent: %v", err)
		a.eval.fail(TuningCategories()...)
		return TuningOutput{}
	}
	a.eval.evaluated(out.Evaluated...)
	return out
}
