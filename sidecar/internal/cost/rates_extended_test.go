package cost

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// drill-02a: the extended-support price travels with the rest of the
// entry; an entry without it leaves the brief unpriced.
func TestConfigRates_CarryExtendedSupport(t *testing.T) {
	compute, ext := 0.5, 0.1
	rates := NewConfigRates(config.CostConfig{Rates: []config.CostRate{
		{Database: "orders", Currency: "USD", AsOf: "2026-11-01", ComputePerHour: &compute,
			ExtendedSupportPerVCPUHour: &ext, VCPUs: 4},
		{Database: "billing", Currency: "EUR", AsOf: "2026-11-01", ComputePerHour: &compute},
	}})
	got, ok := rates.Rates("", "orders")
	if !ok || !got.HasExtendedSupport || got.ExtendedSupportPerVCPUHour != 0.1 ||
		got.VCPUs != 4 || got.Currency != "USD" {
		t.Fatalf("orders = %+v (%v), want the extended-support price", got, ok)
	}
	plain, ok := rates.Rates("", "billing")
	if !ok || plain.HasExtendedSupport || plain.ExtendedSupportPerVCPUHour != 0 {
		t.Fatalf("billing = %+v (%v), want no extended-support price", plain, ok)
	}
}
