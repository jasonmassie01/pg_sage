package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// advisors-03: Azure and AlloyDB became managed-change providers. They
// have no cloud-telemetry source, so cloudTelemetryFor must not hand them
// to the AWS source; Azure's server identity resolves through the ARM
// adapter (read-only), and the startup note no longer promises ARM writes
// beyond the WAL-slot cap.
//
// No concurrent access tests: these wiring helpers hold no shared state.

func TestCloudTelemetryForSkipsProvidersWithoutASource(t *testing.T) {
	for _, tc := range []struct{ provider, host string }{
		{"azure", "x.postgres.database.azure.com"},
		{"azure-flexible", "10.0.0.9"},
		{"alloydb", "10.20.0.7"},
	} {
		rt, resolver := cloudTelemetryFor(context.Background(), cloudCfg(), tc.provider,
			tc.host, "orders", true, config.CloudResourceConfig{}, testCloudDeps())
		if rt != nil || resolver != nil {
			t.Fatalf("%s: runtime=%v resolver=%v, want none", tc.provider, rt, resolver)
		}
	}
}

func TestAzureStartupNoteNamesOnlyTheWALSlotCap(t *testing.T) {
	stubAzureToken(t, nil)
	adapter, note := azureManagedConfig(azureCfg("s", "rg", ""), "azure",
		"srv-1.postgres.database.azure.com")
	if adapter == nil {
		t.Fatalf("adapter = nil, note %q", note)
	}
	for _, want := range []string{"max_slot_wal_keep_size", "srv-1", "proposal"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note %q lacks %q", note, want)
		}
	}
	if strings.Contains(note, "server parameters apply through ARM") {
		t.Fatalf("stale ARM promise: %q", note)
	}
}

func TestManagedChangeResolverPrefersTelemetryThenAzure(t *testing.T) {
	stubAzureToken(t, nil)
	adapter, _ := azureManagedConfig(azureCfg("s", "rg", ""), "azure",
		"srv-1.postgres.database.azure.com")
	got := managedChangeResolver(nil, adapter)
	pr, ok := got.(managedparam.ParameterResolver)
	if !ok || pr == nil {
		t.Fatalf("resolver = %T, want the ARM adapter as a ParameterResolver", got)
	}
	if got := managedChangeResolver(nil, nil); got != nil {
		t.Fatalf("no adapter: resolver = %T, want nil", got)
	}
	telemetry := fakeManagedResolver{}
	if got := managedChangeResolver(telemetry, adapter); got != telemetry {
		t.Fatalf("telemetry resolver must win, got %T", got)
	}
}

type fakeManagedResolver struct{}

func (fakeManagedResolver) Target(context.Context) (managedparam.Target, error) {
	return managedparam.Target{}, nil
}
