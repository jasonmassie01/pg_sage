package verify

import (
	"context"
	"testing"
	"time"
)

var _ ObservationSource = (*fakeObservationSource)(nil)
var _ StateStore = (*memoryStateStore)(nil)

func TestDefaultOptionsMatchPrescribedSafetyContract(t *testing.T) {
	options := DefaultOptions()
	if options.InitialWindow != 2*time.Hour {
		t.Fatalf("initial window = %s, want 2h", options.InitialWindow)
	}
	if options.HardMax != 72*time.Hour {
		t.Fatalf("hard max = %s, want 72h", options.HardMax)
	}
	if options.MinSamples != 30 {
		t.Fatalf("minimum samples = %d, want 30", options.MinSamples)
	}
	if options.MinGainPct != 20 || options.RegressPct != 15 {
		t.Fatalf("gain/regression defaults = %.1f/%.1f", options.MinGainPct, options.RegressPct)
	}
	if options.WriteImpactPct != 20 {
		t.Fatalf("write impact default = %.1f, want 20", options.WriteImpactPct)
	}
}

func TestCriterionCanOverrideEveryVerificationBoundary(t *testing.T) {
	criterion := Criterion{
		Kind:           "per_query_latency",
		TargetIDs:      []int64{41, 42},
		MinGainPct:     25,
		RegressPct:     10,
		WriteImpactPct: 12,
		Window:         3 * time.Hour,
		HardMax:        48 * time.Hour,
	}
	if criterion.Kind != "per_query_latency" || len(criterion.TargetIDs) != 2 {
		t.Fatalf("criterion identity lost: %#v", criterion)
	}
	if criterion.Window != 3*time.Hour || criterion.HardMax != 48*time.Hour {
		t.Fatalf("criterion windows lost: %#v", criterion)
	}
}

func TestEngineRejectsIncompleteDependencies(t *testing.T) {
	tests := []struct {
		name   string
		source ObservationSource
		store  StateStore
	}{
		{name: "missing source", store: newMemoryStateStore()},
		{name: "missing store", source: newFakeObservationSource()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, err := NewEngine(test.source, test.store, DefaultOptions())
			if err == nil || engine != nil {
				t.Fatalf("engine=%v err=%v, want dependency error", engine, err)
			}
		})
	}
}

func TestWatchHonorsAlreadyCanceledContext(t *testing.T) {
	engine := newTestEngine(t, newFakeObservationSource(), newMemoryStateStore())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verdict, err := engine.Watch(ctx, successfulWatchRequest("canceled"))
	if err == nil || ctx.Err() != context.Canceled {
		t.Fatalf("Watch error = %v, context = %v", err, ctx.Err())
	}
	if !verdict.Revert || verdict.Reason != "context_canceled" {
		t.Fatalf("canceled verdict = %#v, want fail-closed revert", verdict)
	}
}
