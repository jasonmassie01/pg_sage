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
	skips := make(map[string]int)
	defer func() { o.logMemorySummary(skips) }()
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
		if !o.askModel(ctx, tc, result, skips) {
			break
		}
	}
}

// askModel runs one table through the model and the admission gates. It
// returns false when the token budget is exhausted (no further table may
// ask).
func (o *Optimizer) askModel(
	ctx context.Context, tc TableContext, result *Result, skips map[string]int,
) bool {
	mem := o.memory.view(ctx, tc)
	recs, tokens, rejections, err := o.analyzeTable(ctx, tc, mem)
	if mem.skipped > 0 {
		skips[tc.Schema+"."+tc.Table] += mem.skipped
		result.MemorySkips += mem.skipped
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

// logMemorySummary reports the cycle's memory skips in one DEBUG line: a
// skip is the expected steady state, not news.
func (o *Optimizer) logMemorySummary(skips map[string]int) {
	if len(skips) == 0 {
		return
	}
	tables := make([]string, 0, len(skips))
	total := 0
	for table, n := range skips {
		tables = append(tables, fmt.Sprintf("%s=%d", table, n))
		total += n
	}
	sort.Strings(tables)
	o.logFn("DEBUG", "optimizer: rejection memory skipped %d what-if evaluation(s) "+
		"of already-measured candidates: %s", total, strings.Join(tables, ", "))
}
