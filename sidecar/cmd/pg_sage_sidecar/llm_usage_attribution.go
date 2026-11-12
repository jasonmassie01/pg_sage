package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// llm-04: the attributed LLM usage ledger. Every registered client reports
// its calls to the accumulator (llmClientObserver); every minute and on
// shutdown the deltas are added to sage.llm_usage in the control
// database, next to fleet-10's budget counters, by database, client role,
// purpose and model. Admission never waits on it.

// llmUsageAttribution is the running accumulator; nil until a mode starts
// the ledger with a control database (then calls are only counted on
// /metrics).
var llmUsageAttribution atomic.Pointer[llm.UsageAccumulator]

// activeLLMAttributionStore reads the ledger for /api/v1/llm/usage.
var activeLLMAttributionStore atomic.Pointer[store.LLMUsageStore]

// llmAttributionRetention is how long attributed usage is kept: 400 days,
// a year of history with a month's margin. The budget counters keep
// llmUsageRetention.
const llmAttributionRetention = 400 * 24 * time.Hour

// llmClientObserver is the observer of a client of database and role: the
// /metrics health series and, once started, the usage accumulator.
func llmClientObserver(database, role string) func(llm.Outcome) {
	health := llmHealth.observer(database, role)
	return func(o llm.Outcome) {
		health(o)
		llmUsageAttribution.Load().Record(database, role, o)
	}
}

// startLLMAttribution starts the accumulator with the operator's prices
// (llm.pricing, read once: a price change needs a restart), prunes rows
// past the retention and flushes every minute until shutdown.
func startLLMAttribution(st *store.LLMUsageStore, pricing map[string]config.LLMPrice) {
	acc := llm.NewUsageAccumulator(llm.DefaultUsageMaxKeys, llm.PricingFrom(pricing),
		logStructuredWrapper)
	cutoff := ledgerDay(time.Now()).Add(-llmAttributionRetention)
	if _, err := st.PruneAttribution(context.Background(), cutoff); err != nil {
		logWarn("llm", "prune LLM usage attribution before %s: %v",
			cutoff.Format(time.DateOnly), err)
	}
	activeLLMAttributionStore.Store(st)
	llmUsageAttribution.Store(acc)
	go runLLMAttribution(shutdownCtx, acc, store.LLMAttributionSink(st, llmUsageIDs.keyOf))
}

// runLLMAttribution flushes acc every llmUsageFlushInterval until ctx ends;
// a failure is logged by the accumulator and retried on the next tick.
func runLLMAttribution(ctx context.Context, acc *llm.UsageAccumulator, sink llm.UsageSink) {
	ticker := time.NewTicker(llmUsageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = acc.Flush(ctx, sink)
		}
	}
}

// flushLLMAttributionOnShutdown writes the last minute of attributed usage.
func flushLLMAttributionOnShutdown(ctx context.Context) {
	acc, st := llmUsageAttribution.Load(), activeLLMAttributionStore.Load()
	if acc == nil || st == nil {
		return
	}
	if err := acc.Flush(ctx, store.LLMAttributionSink(st, llmUsageIDs.keyOf)); err != nil {
		logWarn("shutdown", "final LLM usage attribution flush: %v", err)
	}
}

// LLMUsage returns the attributed usage of the last days UTC days (today
// included) for /api/v1/llm/usage.
func (r *llmClientRegistry) LLMUsage(ctx context.Context, days int) ([]api.LLMUsageRow, error) {
	st := activeLLMAttributionStore.Load()
	if st == nil {
		return nil, api.ErrLLMUsageUnavailable
	}
	since := ledgerDay(time.Now()).AddDate(0, 0, 1-days)
	rows, err := st.Attribution(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("read LLM usage for the last %d days: %w", days, err)
	}
	return llmUsageRows(rows, llmUsageIDs), nil
}

// llmUsageRows names each row's database from its ledger key; a key this
// process no longer knows (and "process") has no name.
func llmUsageRows(rows []store.LLMAttributionRow, keys *llmUsageKeys) []api.LLMUsageRow {
	out := make([]api.LLMUsageRow, 0, len(rows))
	for _, r := range rows {
		name, _ := keys.nameOf(r.DatabaseKey)
		out = append(out, api.LLMUsageRow{Day: r.Day.UTC().Format(time.DateOnly),
			DatabaseKey: r.DatabaseKey, Database: name, LLMRole: r.Role,
			Purpose: r.Purpose, Model: r.Model, PromptTokens: r.PromptTokens,
			CompletionTokens: r.CompletionTokens, ReasoningTokens: r.ReasoningTokens,
			Calls: r.Calls, Failures: r.Failures, EstCostUSD: r.CostUSD, Source: r.Source})
	}
	return out
}
