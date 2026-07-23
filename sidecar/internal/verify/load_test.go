package verify

import (
	"context"
	"errors"
	"testing"
)

func TestOKToApplyNowAllowsLoadAtOrBelowCeilings(t *testing.T) {
	tests := []LoadSample{
		{CPUPct: 0, DataIOPct: 0, LogIOPct: 0},
		{CPUPct: 70, DataIOPct: 70, LogIOPct: 70},
	}
	for _, load := range tests {
		source := newFakeObservationSource()
		source.load = load
		admission, err := newTestEngine(t, source, newMemoryStateStore()).OKToApplyNow(
			context.Background(),
		)
		if err != nil {
			t.Fatalf("OKToApplyNow(%#v): %v", load, err)
		}
		if !admission.OK || admission.Reason != "load_within_ceiling" {
			t.Fatalf("admission for %#v = %#v", load, admission)
		}
	}
}

func TestOKToApplyNowRejectsEachExceededCeiling(t *testing.T) {
	tests := []struct {
		name string
		load LoadSample
		want string
	}{
		{name: "cpu", load: LoadSample{CPUPct: 71}, want: "cpu_ceiling"},
		{name: "data io", load: LoadSample{DataIOPct: 71}, want: "data_io_ceiling"},
		{name: "log io", load: LoadSample{LogIOPct: 71}, want: "log_io_ceiling"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newFakeObservationSource()
			source.load = test.load
			admission, err := newTestEngine(
				t, source, newMemoryStateStore(),
			).OKToApplyNow(context.Background())
			if err != nil {
				t.Fatalf("OKToApplyNow: %v", err)
			}
			if admission.OK || admission.Reason != test.want {
				t.Fatalf("admission = %#v, want refusal %q", admission, test.want)
			}
		})
	}
}

func TestOKToApplyNowLoadErrorsFailClosed(t *testing.T) {
	source := newFakeObservationSource()
	source.loadErr = errObservation
	admission, err := newTestEngine(t, source, newMemoryStateStore()).OKToApplyNow(
		context.Background(),
	)
	if !errors.Is(err, errObservation) {
		t.Fatalf("OKToApplyNow error = %v, want observation error", err)
	}
	if admission.OK || admission.Reason != "load_unavailable" {
		t.Fatalf("error admission = %#v, want fail closed", admission)
	}
}

func TestOKToApplyNowCancellationFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	admission, err := newTestEngine(
		t, newFakeObservationSource(), newMemoryStateStore(),
	).OKToApplyNow(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OKToApplyNow error = %v, want canceled", err)
	}
	if admission.OK {
		t.Fatalf("canceled load check admitted apply: %#v", admission)
	}
}
