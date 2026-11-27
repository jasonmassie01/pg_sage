package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// activeLLMUsageLedger is the running process's ledger (nil until a mode
// starts one); the API status and the budget reset read it.
var activeLLMUsageLedger atomic.Pointer[llmUsageLedger]

// startLLMUsageLedger restores today's LLM usage from the control
// database and starts the 60 s flush. Call it once the clients and the
// fleet budget exist, before the runtimes' first LLM call where the mode
// allows; clients built later are restored as they register.
func startLLMUsageLedger(control *pgxpool.Pool) {
	if control == nil {
		return
	}
	st := store.NewLLMUsageStore(control)
	ledger := newLLMUsageLedger(st, llmClients, fleetLLMBudget, llmUsageIDs)
	ctx := context.Background()
	_ = ledger.restore(ctx) // a failure is logged; the first tick retries
	ledger.prune(ctx)
	activeLLMUsageLedger.Store(ledger)
	go ledger.run(shutdownCtx)
	var pricing map[string]config.LLMPrice
	if cfg != nil {
		pricing = cfg.LLM.Pricing
	}
	startLLMAttribution(st, pricing) // llm-04
}

// flushLLMUsageOnShutdown saves the last minute of usage after the
// runtimes drained, within the store's 5 s statement budget.
func flushLLMUsageOnShutdown() {
	ledger := activeLLMUsageLedger.Load()
	if ledger == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.LLMUsageTimeout)
	defer cancel()
	if err := ledger.flush(ctx); err != nil {
		logWarn("shutdown", "final LLM usage flush: %v", err)
	}
	actx, acancel := context.WithTimeout(context.Background(), store.LLMUsageTimeout)
	defer acancel()
	flushLLMAttributionOnShutdown(actx)
}

// UsageLedgerStatus reports when today's usage was restored and last
// flushed (zero when never), for /api/v1/llm/status.
func (r *llmClientRegistry) UsageLedgerStatus() (time.Time, time.Time) {
	ledger := activeLLMUsageLedger.Load()
	if ledger == nil {
		return time.Time{}, time.Time{}
	}
	return ledger.status()
}
