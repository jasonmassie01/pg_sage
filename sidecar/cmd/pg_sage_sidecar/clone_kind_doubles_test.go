package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
)

// lab-05a: a provider that does not describe itself is a full copy and
// needs clone.allow_full_copy. The reaper tests' adapter double stands in
// for DLE (its ids are dle-N), so it declares DLE's kind.
func (a *reaperAdapter) Describe(context.Context) (clone.Capability, error) {
	return clone.Capability{Kind: clone.KindThinClone, TypicalReady: time.Second}, nil
}

func TestCloneRateFor_PricesOnlyTheNamedDatabase(t *testing.T) {
	price, cu := 1.5, 0.25
	cost := config.CostConfig{Rates: []config.CostRate{
		{Resource: "rds:us-east-1/orders", Currency: "USD", AsOf: "2026-11-01",
			ComputePerHour: &price},
		{Database: "orders", Currency: "EUR", AsOf: "2026-11-02", CUPerHour: &cu,
			ComputeUnits: 4},
		{Database: "storage_only", Currency: "USD", AsOf: "2026-11-02",
			StorageGBMonth: 0.1},
	}}
	got := cloneRateFor(cost, "orders")
	if got == nil || got.PerHour != 1 || got.Currency != "EUR" || got.AsOf != "2026-11-02" ||
		got.Source != "database orders" {
		t.Fatalf("orders rate = %+v, want 1 EUR/h from database orders", got)
	}
	for _, name := range []string{"", "billing", "storage_only"} {
		if r := cloneRateFor(cost, name); r != nil {
			t.Errorf("%q priced as %+v, want unpriced", name, r)
		}
	}
	if r := cloneRateFor(config.CostConfig{}, "orders"); r != nil {
		t.Errorf("no cost.rates priced the clone: %+v", r)
	}
}
