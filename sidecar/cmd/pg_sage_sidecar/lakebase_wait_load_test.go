package main

import (
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

// market-01b: the LakebaseWaitLoad seam reads the retained raw
// wait_event_counters events, keeps their class and event labels (which
// NormalizeMetrics drops) and hands them to lakebaseobs.WaitLoad.

func waitCounterEvent(t *testing.T, scope lakebaseobs.Scope, at, received time.Time,
	class, event string, waited time.Duration) lakebaseobs.Event {
	t.Helper()
	units := waited.Seconds() / lakebaseobs.WaitEventTimeUnit.Seconds()
	return historyWiringEvent(t, scope, lakebaseobs.WaitCounters, at, received,
		map[string]string{"wait_event_id": class + "/" + event, "wait_class_name": class,
			"wait_event_name": event, "wait_event_count": "3",
			"wait_event_time": strconv.FormatFloat(units, 'f', -1, 64)})
}

func TestLakebaseWaitLoadReadsRawCountersWithLabels(t *testing.T) {
	f := newHistoryWiringFixture(t)
	rt, scope, _ := f.registration(t)
	scope.AllowCompute = true
	runtime, _ := f.runtime(t, scope)
	at := f.now.Add(-2 * time.Minute) // each fixture interval is the minute before at
	f.insert(t, scope, runtime,
		waitCounterEvent(t, scope, at, f.now, "LWLock", "WALWrite", 30*time.Second),
		waitCounterEvent(t, scope, at, f.now, "CPU", "", 60*time.Second),
		historyWiringEvent(t, scope, lakebaseobs.DatabaseCounters, at, f.now, nil))
	state := &lakebaseTelemetryState{runtime: runtime, store: f.store}
	registerHistoryWiring(t, rt.spec.Name, state)
	got, err := lakebaseWaitLoad(f.ctx, rt.spec.Name, f.now.Add(-time.Hour), f.now)
	if err != nil {
		t.Fatalf("lakebaseWaitLoad: %v", err)
	}
	if len(got.Points) != 1 || got.Skipped != 0 || got.Truncated {
		t.Fatalf("got %+v, want one interval, nothing skipped, not truncated", got)
	}
	p := got.Points[0]
	if !p.T.Equal(at) || !p.From.Equal(at.Add(-time.Minute)) ||
		math.Abs(p.ByClass["LWLock"]-0.5) > 1e-9 || math.Abs(p.ByClass["CPU"]-1) > 1e-9 {
		t.Fatalf("point %+v, want LWLock 0.5 and CPU 1.0 over the minute before %v", p, at)
	}
	if len(got.Events) != 2 || got.Events[1].WaitClass != "LWLock" ||
		got.Events[1].WaitEvent != "WALWrite" || got.Events[1].Count != 3 {
		t.Fatalf("events %+v, want the raw labels LWLock/WALWrite kept", got.Events)
	}
}

func TestLakebaseWaitLoadUnavailableRegistration(t *testing.T) {
	name := "unavailable-wait-load-" + uuid.NewString()
	now := time.Now().UTC()
	for label, state := range map[string]*lakebaseTelemetryState{
		"unregistered": nil, "empty state": {},
	} {
		if state != nil {
			registerHistoryWiring(t, name, state)
		}
		_, err := lakebaseWaitLoad(t.Context(), name, now.Add(-time.Hour), now)
		if !errors.Is(err, lakebaseobs.ErrHistoryUnavailable) {
			t.Fatalf("%s: err %v, want ErrHistoryUnavailable", label, err)
		}
	}
}
