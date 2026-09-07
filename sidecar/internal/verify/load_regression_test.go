package verify

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestCatalogConnectionCountIsNotHostLoadTelemetry(t *testing.T) {
	queryer := &fakeRowQuerier{}
	source := &PostgresObservationSource{queryer: queryer}
	load, err := source.CurrentLoad(t.Context())
	if !errors.Is(err, ErrLoadTelemetryUnavailable) || load != (LoadSample{}) {
		t.Fatalf("catalog source fabricated host telemetry: %#v, %v", load, err)
	}
	if queryer.next != 0 {
		t.Fatal("connection count queried as a CPU/I/O proxy")
	}
	engine := newTestEngine(t, source, newMemoryStateStore())
	admission, err := engine.OKToApplyNow(t.Context())
	if admission.OK || admission.Reason != "load_unavailable" ||
		!errors.Is(err, ErrLoadTelemetryUnavailable) {
		t.Fatalf("missing host telemetry admitted automatic change: %#v %v", admission, err)
	}
}

func TestLoadGateRejectsNonfiniteAndOutOfRangeTelemetry(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 101} {
		for metric := range 3 {
			source := newFakeObservationSource()
			values := []*float64{&source.load.CPUPct, &source.load.DataIOPct, &source.load.LogIOPct}
			*values[metric] = bad
			admission, err := newTestEngine(t, source, newMemoryStateStore()).OKToApplyNow(t.Context())
			if admission.OK || admission.Reason != "load_invalid" || err == nil {
				t.Fatalf("bad load %v metric %d admitted: %#v %v", bad, metric, admission, err)
			}
		}
	}
}

func TestCatalogLoadCancellationIsPreserved(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewPostgresObservationSource(nil).CurrentLoad(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
