//go:build integration

package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/store"
)

// fleet-10 CHECK-09 on real PostgreSQL: spend, flush to sage.llm_usage,
// rebuild the whole LLM runtime as a restart does, start the ledger from
// the same control database, and /llm/status reports the same usage.
func TestLLMUsageLedgerDB_RestartKeepsStatusUsage(t *testing.T) {
	pool := policyCeilingPool(t)
	ctx := context.Background()
	clear := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.llm_usage
			WHERE day = (now() AT TIME ZONE 'UTC')::date`)
	}
	clear()
	t.Cleanup(clear)
	t.Cleanup(func() { activeLLMUsageLedger.Store(nil) })

	fleetLedgerRuntime(t, 400)
	general, _ := newFleetDBLLMClients("orders", true)
	if _, _, err := general.Chat(ctx, "sys", "spend 400", 10); err != nil {
		t.Fatalf("chat: %v", err)
	}
	startLLMUsageLedger(pool)
	before := llmClients.TokenStatus()
	flushLLMUsageOnShutdown()
	if _, flushed := llmClients.UsageLedgerStatus(); flushed.IsZero() {
		t.Fatal("the shutdown flush did not record a flush time")
	}

	fleetLedgerRuntime(t, 400)
	activeLLMUsageLedger.Store(nil)
	startLLMUsageLedger(pool)
	rebuilt, _ := newFleetDBLLMClients("orders", true)
	after := llmClients.TokenStatus()
	for _, key := range []string{"orders/general", "orders/fleet_budget"} {
		if before[key].TokensUsed != 400 || after[key].TokensUsed != before[key].TokensUsed {
			t.Fatalf("%s used before %d, after restart %d, want 400 both", key,
				before[key].TokensUsed, after[key].TokensUsed)
		}
	}
	if rebuilt.TokensUsedToday() != 400 {
		t.Fatalf("rebuilt client = %d, want 400", rebuilt.TokensUsedToday())
	}
	if restored, _ := llmClients.UsageLedgerStatus(); restored.IsZero() {
		t.Fatal("usage_restored_at must be set after a restore")
	}
	rows, err := store.NewLLMUsageStore(pool).Load(ctx, ledgerToday())
	if err != nil || len(rows) != 3 {
		t.Fatalf("ledger rows = %+v (%v), want fleet, general and shared", rows, err)
	}
}

// CHECK-10 locally: with the control database closed, LLM calls still run
// and the ledger warns once.
func TestLLMUsageLedgerDB_ClosedControlDatabaseFailsOpen(t *testing.T) {
	pool := policyCeilingPool(t)
	fleetLedgerRuntime(t, 20)
	general, _ := newFleetDBLLMClients("orders", true)
	l, w := testLedger(store.NewLLMUsageStore(pool), newLLMUsageKeys())
	pool.Close()
	ctx := context.Background()
	if err := l.restore(ctx); err == nil {
		t.Fatal("restore from a closed pool must fail")
	}
	for i := 0; i < 2; i++ {
		if _, _, err := general.Chat(ctx, "sys", fmt.Sprintf("still runs %d", i), 10); err != nil {
			t.Fatalf("chat %d with the control database down: %v", i, err)
		}
		if err := l.flush(ctx); err == nil {
			t.Fatal("flush to a closed pool must fail")
		}
	}
	if w.count() != 1 {
		t.Fatalf("warnings = %d (%v), want 1", w.count(), w.lines)
	}
	if general.TokensUsedToday() != 40 {
		t.Fatalf("local accounting = %d, want 40", general.TokensUsedToday())
	}
}
