package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// advisors-03: Azure and AlloyDB became managed-change providers; clouds-09
// gave them their own telemetry sources. cloudTelemetryFor must never hand
// them to the AWS source nor return a telemetry resolver for them: Azure's
// server identity resolves through the ARM adapter (read-only), AlloyDB's
// changes stay proposals, and the startup note no longer promises ARM
// writes beyond the WAL-slot cap.
//
// No concurrent access tests: these wiring helpers hold no shared state.

func TestCloudTelemetryForKeepsAzureAndAlloyDBOffTheAWSSource(t *testing.T) {
	for _, tc := range []struct{ provider, host, want string }{
		{"azure", "x.postgres.database.azure.com", "azure"},
		{"azure-flexible", "10.0.0.9", "azure"},
		{"alloydb", "10.20.0.7", "alloydb"},
	} {
		rt, resolver := cloudTelemetryFor(context.Background(), cloudCfg(), tc.provider,
			tc.host, "orders", true, config.CloudResourceConfig{}, testCloudDeps())
		if rt == nil || resolver != nil {
			t.Fatalf("%s: runtime=%v resolver=%v, want a runtime and no resolver",
				tc.provider, rt, resolver)
		}
		if st := rt.Status(); st.Provider != tc.want || st.Available {
			t.Fatalf("%s: status %+v, want an unnamed %s runtime", tc.provider, st, tc.want)
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

// Without the ARM adapter the note says what is lost (the parameter reads
// for proposals and the WAL-slot cap) and never that pg_sage would apply
// server parameters once the azure: section is set.
func TestAzureNoAdapterNotesDoNotPromiseParameterWrites(t *testing.T) {
	stubAzureToken(t, nil)
	for _, tc := range []struct{ sub, rg, host, want string }{
		{"", "", "srv-1.postgres.database.azure.com", "azure.subscription_id"},
		{"s", "rg", "10.0.0.5", "azure.server_name"},
	} {
		adapter, note := azureManagedConfig(azureCfg(tc.sub, tc.rg, ""), "azure", tc.host)
		if adapter != nil {
			t.Fatalf("%s: adapter = %T, want none", tc.want, adapter)
		}
		for _, want := range []string{tc.want, "fill-in templates", "WAL-slot cap"} {
			if !strings.Contains(note, want) {
				t.Fatalf("note %q lacks %q", note, want)
			}
		}
		if strings.Contains(note, "apply them") {
			t.Fatalf("stale promise that pg_sage applies parameters: %q", note)
		}
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
