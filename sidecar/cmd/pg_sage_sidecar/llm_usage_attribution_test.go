package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// llm-04: every registered client feeds the attributed usage accumulator
// and the purpose-labelled pg_sage_llm_tokens_total counter.
//
// The database round trip is covered in internal/store and internal/llm
// (integration); here the transport is a fake HTTP endpoint.

func withAttribution(t *testing.T) *llm.UsageAccumulator {
	t.Helper()
	acc := llm.NewUsageAccumulator(llm.DefaultUsageMaxKeys, llm.PricingFrom(nil),
		func(string, string, ...any) {})
	old := llmUsageAttribution.Swap(acc)
	t.Cleanup(func() { llmUsageAttribution.Store(old) })
	return acc
}

// CHECK-20: in fleet mode each database's client reports under its own
// database and purpose labels, and into the accumulator.
func TestLLMAttributionPerDatabaseInFleetMode(t *testing.T) {
	installHealthFleet(t, "fleet")
	withLLMHealth(t, llmMetricsFake(t).URL, 100_000)
	acc := withAttribution(t)
	orders := llmClients.newClient(llmRoleGeneral, "orders", true)
	billing := llmClients.newClient(llmRoleGeneral, "billing", true)
	_, _, _ = orders.Chat(llm.WithPurpose(context.Background(), "briefing"), "s", "ok-1", 50)
	_, _, _ = billing.Chat(llm.WithPurpose(context.Background(), "explain"), "s", "ok-2", 50)
	_, _, _ = billing.Chat(context.Background(), "s", "ok-3", 50)
	e := lintScrape(t, scrapeMetrics(t))
	for _, c := range []struct{ db, purpose string }{{"orders", "briefing"},
		{"billing", "explain"}, {"billing", llm.PurposeUnlabeled}} {
		key := llmKey("general", c.db, "purpose", c.purpose, "kind", "prompt")
		if got := llmSample(t, e, "pg_sage_llm_tokens_total", key); got != 7 {
			t.Errorf("tokens_total%v = %v, want 7", key, got)
		}
	}
	byKey := map[string]int64{}
	for k, v := range acc.Pending() {
		byKey[k.Database+"/"+k.Role+"/"+k.Purpose+"/"+k.Model] += v.PromptTokens
	}
	want := map[string]int64{"orders/general/briefing/test-model": 7,
		"billing/general/explain/test-model": 7, "billing/general/unlabeled/test-model": 7}
	if len(byKey) != len(want) {
		t.Fatalf("accumulator keys = %v, want %v", byKey, want)
	}
	for k, n := range want {
		if byKey[k] != n {
			t.Errorf("accumulator %s = %d, want %d", k, byKey[k], n)
		}
	}
}

// Nil/empty: without a running ledger the clients still serve and the
// counter still counts; nothing is accumulated.
func TestLLMAttributionOffWithoutLedger(t *testing.T) {
	installHealthFleet(t, "standalone")
	withLLMHealth(t, llmMetricsFake(t).URL, 100_000)
	old := llmUsageAttribution.Swap(nil)
	t.Cleanup(func() { llmUsageAttribution.Store(old) })
	client := llmClients.newClient(llmRoleGeneral, "", true)
	if _, _, err := client.Chat(llm.WithPurpose(context.Background(), "facts"), "s", "ok",
		50); err != nil {
		t.Fatalf("chat without a ledger: %v", err)
	}
	e := lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_tokens_total",
		llmKey("general", "", "purpose", "facts", "kind", "completion")); got != 3 {
		t.Fatalf("completion tokens = %v, want 3", got)
	}
}

// The API rows name each database by its ledger key; an id this process
// no longer knows keeps only the key.
func TestLLMUsageRowsResolveDatabaseNames(t *testing.T) {
	keys := newLLMUsageKeys()
	keys.setID("orders", 7)
	cost := 0.5
	day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	rows := llmUsageRows([]store.LLMAttributionRow{
		{Day: day, DatabaseKey: "id:7", Role: "general", Purpose: "briefing", Model: "m",
			PromptTokens: 10, Calls: 1, CostUSD: &cost, Source: store.LLMSourceProvider},
		{Day: day, DatabaseKey: "id:99", Role: "optimizer", Purpose: "query_tuning",
			Model: "o", Calls: 1, Source: store.LLMSourceEstimate},
		{Day: day, DatabaseKey: "name:billing", Role: "general", Purpose: "explain",
			Model: "m", Calls: 1, Source: store.LLMSourceProvider},
		{Day: day, DatabaseKey: "process", Role: "general", Purpose: "probe",
			Model: "m", Calls: 1, Source: store.LLMSourceProvider},
	}, keys)
	want := []api.LLMUsageRow{
		{Day: "2026-11-12", DatabaseKey: "id:7", Database: "orders", LLMRole: "general",
			Purpose: "briefing", Model: "m", PromptTokens: 10, Calls: 1, EstCostUSD: &cost,
			Source: "provider"},
		{Day: "2026-11-12", DatabaseKey: "id:99", LLMRole: "optimizer",
			Purpose: "query_tuning", Model: "o", Calls: 1, Source: "estimate"},
		{Day: "2026-11-12", DatabaseKey: "name:billing", Database: "billing",
			LLMRole: "general", Purpose: "explain", Model: "m", Calls: 1, Source: "provider"},
		{Day: "2026-11-12", DatabaseKey: "process", LLMRole: "general", Purpose: "probe",
			Model: "m", Calls: 1, Source: "provider"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d", rows, len(want))
	}
	for i := range want {
		got, w := rows[i], want[i]
		if got.EstCostUSD != w.EstCostUSD && (got.EstCostUSD == nil || w.EstCostUSD == nil ||
			*got.EstCostUSD != *w.EstCostUSD) {
			t.Errorf("row %d cost = %v, want %v", i, got.EstCostUSD, w.EstCostUSD)
		}
		got.EstCostUSD, w.EstCostUSD = nil, nil
		if got != w {
			t.Errorf("row %d = %+v, want %+v", i, got, w)
		}
	}
}

// Error propagation: the API says the ledger is unavailable until a mode
// starts it with a control database.
func TestLLMUsageUnavailableWithoutLedger(t *testing.T) {
	old := activeLLMAttributionStore.Swap(nil)
	t.Cleanup(func() { activeLLMAttributionStore.Store(old) })
	_, err := llmClients.LLMUsage(context.Background(), 30)
	if !errors.Is(err, api.ErrLLMUsageUnavailable) {
		t.Fatalf("LLMUsage without a ledger = %v, want ErrLLMUsageUnavailable", err)
	}
}
