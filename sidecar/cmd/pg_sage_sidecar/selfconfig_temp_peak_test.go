package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
	"github.com/pg-sage/sidecar/internal/sre"
)

// footprint-04: the temp-file threshold's soak samples the reactive
// detector's largest per-window temp growth since the previous pass. The
// detector the investigator builds is handed to the database runtime, and
// each self-configuration pass takes its peak into the evidence.

// No concurrent access tests here: the detector is handed over on the
// runtime's build goroutine before the self-config loop starts, and the
// detector's own peak accessor is tested concurrently in internal/sre.

func TestSRETriggerSource_HandsOverTheDetector(t *testing.T) {
	var got *sre.ReactiveDetector
	_, err := sreTriggerSource(sreInvestigatorDeps{runner: noProbes{}, name: "w",
		settings: config.DefaultConfig().SRE, logFn: func(string, string, ...any) {},
		onDetector: func(d *sre.ReactiveDetector) { got = d }})
	if err != nil || got == nil {
		t.Fatalf("detector not handed over: %v %v", got, err)
	}
	if v, ok := got.TakeTempPeak(); ok || v != 0 {
		t.Fatalf("a fresh detector reported a peak %v", v)
	}
	// Without a receiver nothing is handed over and nothing breaks.
	if _, err := sreTriggerSource(sreInvestigatorDeps{runner: noProbes{}, name: "w",
		settings: config.DefaultConfig().SRE}); err != nil {
		t.Fatalf("trigger source without a receiver: %v", err)
	}
}

func TestSRETriggerSource_InvalidDetectorIsNotHandedOver(t *testing.T) {
	s := config.DefaultConfig().SRE
	s.Detectors.LWLockWaiters = -1
	called := false
	_, err := sreTriggerSource(sreInvestigatorDeps{runner: noProbes{}, name: "w",
		settings: s, onDetector: func(*sre.ReactiveDetector) { called = true }})
	if err == nil || called {
		t.Fatalf("invalid detector handed over (%v) or accepted: %v", called, err)
	}
}

func peakRunner(peak func() (float64, bool)) *selfConfigRunner {
	ev := selfconfig.Evidence{At: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		TempBytesPerSecond: selfconfig.Known(1 << 20)}
	return &selfConfigRunner{name: "orders", tempPeak: peak,
		gather: func(context.Context) (selfconfig.Evidence, error) { return ev, nil }}
}

func TestSelfConfigEvidenceTakesTheTempPeak(t *testing.T) {
	takes := 0
	r := peakRunner(func() (float64, bool) { takes++; return 700 << 20, true })
	ev, err := r.evidence(context.Background())
	if err != nil || takes != 1 {
		t.Fatalf("evidence: %v, %d takes", err, takes)
	}
	if ev.TempWindowPeakBytes != selfconfig.Known(700<<20) ||
		ev.TempBytesPerSecond != selfconfig.Known(1<<20) {
		t.Fatalf("evidence %+v", ev)
	}
	for name, peak := range map[string]func() (float64, bool){
		"no detector":    nil,
		"nothing polled": func() (float64, bool) { return 0, false },
	} {
		ev, err := peakRunner(peak).evidence(context.Background())
		if err != nil || ev.TempWindowPeakBytes.Known {
			t.Fatalf("%s: peak %+v (%v)", name, ev.TempWindowPeakBytes, err)
		}
	}
}

func TestSelfConfigEvidenceKeepsTheGatherError(t *testing.T) {
	boom := errors.New("catalog scan: permission denied")
	r := peakRunner(func() (float64, bool) { return 5 << 20, true })
	r.gather = func(context.Context) (selfconfig.Evidence, error) {
		return selfconfig.Evidence{}, boom
	}
	ev, err := r.evidence(context.Background())
	if !errors.Is(err, boom) || ev.TempWindowPeakBytes != selfconfig.Known(5<<20) {
		t.Fatalf("evidence %+v err %v", ev, err)
	}
}

func TestSelfConfigRunnerReadsTheRuntimeDetector(t *testing.T) {
	d, err := sre.NewReactiveDetector(noProbes{}, "orders", sre.DefaultDetectorConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: "orders"},
		cfg: config.DefaultConfig(), tempDetector: d}
	if r := rt.newSelfConfigRunner(); r.tempPeak == nil {
		t.Fatal("the runner does not read the runtime's detector")
	}
	rt.tempDetector = nil
	if r := rt.newSelfConfigRunner(); r.tempPeak != nil {
		t.Fatal("a runtime without a detector handed the runner a peak source")
	}
}
