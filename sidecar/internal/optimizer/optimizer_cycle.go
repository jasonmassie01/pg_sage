package optimizer

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// analyzeTables asks the model about each table (re-emitting open
// recommendations instead) and accumulates the outcome in result.
func (o *Optimizer) analyzeTables(
	ctx context.Context, contexts []TableContext, result *Result,
) {
	cyc := newCycleMemory()
	defer func() { o.logMemorySummary(cyc) }()
	for _, tc := range contexts {
		if o.breaker.ShouldSkip(tc.Schema, tc.Table) {
			o.logFn("optimizer",
				"circuit open for %s.%s, skipping", tc.Schema, tc.Table,
			)
			continue
		}
		if open, hasOpen := o.openRecommendations(ctx, tc); hasOpen {
			// Re-emit the pending candidates so the analyzer keeps them
			// open instead of resolving them for not reappearing (C06).
			o.logFn("optimizer",
				"skipping %s.%s: %d open index recommendation(s) re-emitted",
				tc.Schema, tc.Table, len(open),
			)
			result.Recommendations = append(result.Recommendations, open...)
			continue
		}
		if !o.askModel(ctx, tc, result, cyc) {
			break
		}
	}
}

// askModel runs one table through the model and the admission gates. It
// returns false when the token budget is exhausted (no further table may
// ask).
func (o *Optimizer) askModel(
	ctx context.Context, tc TableContext, result *Result, cyc *cycleMemory,
) bool {
	table := tc.Schema + "." + tc.Table
	if !operatorRequested(ctx) && o.memory.skipModel(tc) {
		cyc.model = append(cyc.model, table)
		result.LLMCallsSkipped++
		o.llmSkips.Add(1)
		return true
	}
	mem := o.memory.view(ctx, tc)
	recs, tokens, rejections, err := o.analyzeTable(ctx, tc, mem)
	if mem.skipped > 0 {
		cyc.whatIf[table] += mem.skipped
		result.MemorySkips += mem.skipped
		o.whatIfSkips.Add(int64(mem.skipped))
	}
	if err != nil {
		if isBudgetExhausted(err) {
			o.logFn("WARN",
				"optimizer: daily token budget exhausted, "+
					"skipping remaining tables (%s.%s and after)",
				tc.Schema, tc.Table,
			)
			result.BudgetExhausted = true
			return false
		}
		o.logFn("optimizer",
			"table %s.%s: %v", tc.Schema, tc.Table, err,
		)
		if shouldTripTableCircuit(err) {
			o.breaker.RecordFailure(tc.Schema, tc.Table)
		}
		return true
	}
	if len(recs) > 0 {
		o.breaker.RecordSuccess(tc.Schema, tc.Table)
	}
	result.TokensUsed += tokens
	result.Rejections += rejections
	result.Recommendations = append(result.Recommendations, recs...)
	return true
}

// cycleMemory collects one cycle's rejection-memory skips for its summary.
type cycleMemory struct {
	whatIf map[string]int // what-if evaluations skipped, by table
	model  []string       // tables the model was not asked about
}

func newCycleMemory() *cycleMemory {
	return &cycleMemory{whatIf: make(map[string]int)}
}

// logMemorySummary reports the cycle's memory skips in one DEBUG line: a
// skip is the expected steady state, not news.
func (o *Optimizer) logMemorySummary(cyc *cycleMemory) {
	if len(cyc.whatIf) == 0 && len(cyc.model) == 0 {
		return
	}
	tables := make([]string, 0, len(cyc.whatIf))
	total := 0
	for table, n := range cyc.whatIf {
		tables = append(tables, fmt.Sprintf("%s=%d", table, n))
		total += n
	}
	sort.Strings(tables)
	model := append([]string(nil), cyc.model...)
	sort.Strings(model)
	o.logFn("DEBUG", "optimizer: rejection memory skipped %d what-if evaluation(s) "+
		"of already-measured candidates (%s) and the model for %d table(s) (%s)",
		total, strings.Join(tables, ", "), len(model), strings.Join(model, ", "))
}

// MemoryStats are the optimizer's rejection-memory counters since start.
type MemoryStats struct {
	WhatIfSkipped   int64 // what-if evaluations skipped (already measured)
	LLMCallsSkipped int64 // model calls skipped (wasted-proposal streak)
}

// MemoryStats returns the rejection-memory counters (zero for nil).
func (o *Optimizer) MemoryStats() MemoryStats {
	if o == nil {
		return MemoryStats{}
	}
	return MemoryStats{WhatIfSkipped: o.whatIfSkips.Load(),
		LLMCallsSkipped: o.llmSkips.Load()}
}
