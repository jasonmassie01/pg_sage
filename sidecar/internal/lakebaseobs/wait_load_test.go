package lakebaseobs

import (
	"errors"
	"math"
	"strconv"
	"testing"
	"time"
)

// WaitLoad (market-01b): Lakebase wait_event_counters intervals become
// average active sessions by wait class. The tests write wait_event_time
// in WaitEventTimeUnit so they hold whatever unit CHECK-40 confirms.

// No concurrent access tests: WaitLoad is a pure function of its inputs.

var waitT0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// waitUnits is d written in wait_event_time units.
func waitUnits(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds()/WaitEventTimeUnit.Seconds(), 'f', -1, 64)
}

func waitEvent(from, to time.Time, class, event string, waited time.Duration) Event {
	return Event{Stream: WaitCounters, ComputeID: "compute-a", OccurredAt: to,
		Fields: map[string]string{"previous_ts": from.Format(time.RFC3339Nano),
			"ts": to.Format(time.RFC3339Nano), "wait_class_name": class,
			"wait_event_name": event, "wait_event_count": "4",
			"wait_event_time": waitUnits(waited)}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestWaitLoad_HappyPathAverageActiveSessionsByClass(t *testing.T) {
	mid, end := waitT0.Add(30*time.Second), waitT0.Add(60*time.Second)
	events := []Event{
		waitEvent(mid, end, "LWLock", "WALWrite", 6*time.Second),
		waitEvent(waitT0, mid, "LWLock", "WALWrite", 15*time.Second),
		waitEvent(waitT0, mid, "CPU", "", 30*time.Second),
	}
	got, err := WaitLoad(events, waitT0, end)
	if err != nil {
		t.Fatalf("WaitLoad: %v", err)
	}
	if len(got.Points) != 2 || got.Skipped != 0 {
		t.Fatalf("points=%d skipped=%d, want 2 points and 0 skipped: %+v",
			len(got.Points), got.Skipped, got)
	}
	p0, p1 := got.Points[0], got.Points[1]
	if !p0.From.Equal(waitT0) || !p0.T.Equal(mid) || !p1.From.Equal(mid) || !p1.T.Equal(end) {
		t.Fatalf("points out of order or mis-timed: %+v", got.Points)
	}
	if !near(p0.ByClass["LWLock"], 0.5) || !near(p0.ByClass["CPU"], 1.0) ||
		!near(p0.Sessions, 1.5) || len(p0.ByClass) != 2 {
		t.Fatalf("first interval = %+v, want LWLock 0.5 + CPU 1.0 = 1.5", p0)
	}
	if !near(p1.ByClass["LWLock"], 0.2) || !near(p1.Sessions, 0.2) || len(p1.ByClass) != 1 {
		t.Fatalf("second interval = %+v, want LWLock 0.2 only", p1)
	}
	if len(got.Classes) != 2 || got.Classes[0] != "CPU" || got.Classes[1] != "LWLock" {
		t.Fatalf("classes = %v, want CPU (30 s) before LWLock (21 s)", got.Classes)
	}
	if len(got.Events) != 2 || got.Events[0].WaitClass != "CPU" ||
		got.Events[1].WaitClass != "LWLock" || got.Events[1].WaitEvent != "WALWrite" ||
		!near(got.Events[1].WaitSeconds, 21) || !near(got.Events[0].WaitSeconds, 30) ||
		!near(got.Events[1].Count, 8) {
		t.Fatalf("events = %+v, want CPU 30 s then LWLock/WALWrite 21 s x8", got.Events)
	}
}

func TestWaitLoad_ComputesOfOneIntervalAreSummed(t *testing.T) {
	end := waitT0.Add(time.Minute)
	a := waitEvent(waitT0, end, "IO", "DataFileRead", 30*time.Second)
	b := waitEvent(waitT0, end, "IO", "DataFileRead", 90*time.Second)
	b.ComputeID = "compute-b"
	got, err := WaitLoad([]Event{a, b}, waitT0, end)
	if err != nil || len(got.Points) != 1 || !near(got.Points[0].ByClass["IO"], 2.0) {
		t.Fatalf("got %+v err %v, want one interval with IO 2.0 (0.5 + 1.5)", got, err)
	}
}

func TestWaitLoad_InvalidIntervalsAreSkippedAndCounted(t *testing.T) {
	end := waitT0.Add(time.Minute)
	bad := map[string]func(e *Event){
		"previous after occurred": func(e *Event) {
			e.Fields["previous_ts"] = end.Add(time.Second).Format(time.RFC3339Nano)
		},
		"zero length":  func(e *Event) { e.Fields["previous_ts"] = end.Format(time.RFC3339Nano) },
		"bad previous": func(e *Event) { e.Fields["previous_ts"] = "yesterday" },
		"no previous":  func(e *Event) { delete(e.Fields, "previous_ts") },
		"text count":   func(e *Event) { e.Fields["wait_event_count"] = "many" },
		"text time":    func(e *Event) { e.Fields["wait_event_time"] = "slow" },
		"negative":     func(e *Event) { e.Fields["wait_event_time"] = "-5" },
		"NaN time":     func(e *Event) { e.Fields["wait_event_time"] = "NaN" },
		"no time":      func(e *Event) { delete(e.Fields, "wait_event_time") },
		"no class":     func(e *Event) { e.Fields["wait_class_name"] = " " },
		"masked class": func(e *Event) { e.Fields["wait_class_name"] = "<REDACTED>" },
		"other stream": func(e *Event) { e.Stream = DatabaseCounters },
	}
	events := []Event{waitEvent(waitT0, end, "CPU", "", 30*time.Second)}
	for name, mutate := range bad {
		e := waitEvent(waitT0, end, "Lock", name, 60*time.Second)
		mutate(&e)
		events = append(events, e)
	}
	got, err := WaitLoad(events, waitT0, end)
	if err != nil {
		t.Fatalf("WaitLoad: %v", err)
	}
	if got.Skipped != len(bad) {
		t.Errorf("skipped = %d, want %d (one per bad interval)", got.Skipped, len(bad))
	}
	if len(got.Points) != 1 || len(got.Points[0].ByClass) != 1 ||
		!near(got.Points[0].ByClass["CPU"], 0.5) {
		t.Fatalf("points = %+v, want only the valid CPU 0.5 interval", got.Points)
	}
}

func TestWaitLoad_EmptyAndNilGiveEmptyLists(t *testing.T) {
	for name, events := range map[string][]Event{"nil": nil, "empty": {}} {
		got, err := WaitLoad(events, waitT0, waitT0.Add(time.Hour))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Points == nil || got.Classes == nil || got.Events == nil ||
			len(got.Points)+len(got.Classes)+len(got.Events)+got.Skipped != 0 {
			t.Fatalf("%s: %+v, want empty non-nil lists and nothing skipped", name, got)
		}
	}
}

func TestWaitLoad_InvalidWindowIsAnError(t *testing.T) {
	for name, w := range map[string][2]time.Time{
		"until before since": {waitT0, waitT0.Add(-time.Second)},
		"empty window":       {waitT0, waitT0},
		"zero since":         {{}, waitT0},
	} {
		if _, err := WaitLoad(nil, w[0], w[1]); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestWaitLoad_WindowBoundaries(t *testing.T) {
	from, to := waitT0, waitT0.Add(10*time.Minute)
	minute := time.Minute
	events := []Event{
		waitEvent(from, from.Add(minute), "Starts", "at-from", 30*time.Second),
		waitEvent(from.Add(-minute), from, "Ends", "at-from", 30*time.Second),
		waitEvent(to.Add(-minute), to, "Ends", "at-to", 30*time.Second),
		waitEvent(to, to.Add(minute), "Starts", "at-to", 30*time.Second),
	}
	got, err := WaitLoad(events, from, to)
	if err != nil {
		t.Fatalf("WaitLoad: %v", err)
	}
	if len(got.Points) != 2 || got.Skipped != 0 {
		t.Fatalf("got %+v, want 2 in-window intervals and nothing skipped", got)
	}
	if !got.Points[0].From.Equal(from) || !got.Points[1].T.Equal(to) {
		t.Fatalf("points = %+v, want the one starting at since and the one ending at until",
			got.Points)
	}
	for _, e := range got.Events {
		if (e.WaitClass == "Ends" && e.WaitEvent == "at-from") ||
			(e.WaitClass == "Starts" && e.WaitEvent == "at-to") {
			t.Fatalf("out-of-window interval counted: %+v", got.Events)
		}
	}
}

func TestWaitLoad_EventsAreCappedHeaviestFirst(t *testing.T) {
	end := waitT0.Add(time.Minute)
	var events []Event
	for i := 0; i < maxWaitLoadEvents+5; i++ {
		events = append(events,
			waitEvent(waitT0, end, "LWLock", "e"+strconv.Itoa(i), time.Duration(i+1)*time.Second))
	}
	got, err := WaitLoad(events, waitT0, end)
	if err != nil || len(got.Events) != maxWaitLoadEvents {
		t.Fatalf("events = %d err %v, want %d", len(got.Events), err, maxWaitLoadEvents)
	}
	last := "e" + strconv.Itoa(maxWaitLoadEvents+4)
	if got.Events[0].WaitEvent != last || !near(got.Events[0].WaitSeconds,
		float64(maxWaitLoadEvents+5)) {
		t.Fatalf("heaviest event = %+v, want %s", got.Events[0], last)
	}
}

func TestErrHistoryUnavailableIsDistinguishable(t *testing.T) {
	wrapped := errors.Join(errors.New("context"), ErrHistoryUnavailable)
	if !errors.Is(wrapped, ErrHistoryUnavailable) ||
		ErrHistoryUnavailable.Error() != "history source unavailable" {
		t.Fatalf("ErrHistoryUnavailable = %q", ErrHistoryUnavailable)
	}
}

// Computes need not share interval edges: the current load sums each
// compute's newest interval that overlaps the newest one, so a replica
// reporting a few seconds later does not hide the primary's load, and a
// compute that stopped reporting earlier in the window is not counted.
func TestWaitLoad_CurrentLoadSumsEachComputesNewestInterval(t *testing.T) {
	end := waitT0.Add(time.Minute)
	primary := waitEvent(waitT0, end, "IO", "DataFileRead", 30*time.Second)
	older := waitEvent(waitT0.Add(-time.Minute), waitT0, "IO", "DataFileRead", 60*time.Second)
	replica := waitEvent(waitT0.Add(5*time.Second), end.Add(5*time.Second), "IO",
		"DataFileRead", 90*time.Second)
	replica.ComputeID = "compute-b"
	stopped := waitEvent(waitT0.Add(-time.Minute), waitT0, "Lock", "relation", 60*time.Second)
	stopped.ComputeID = "compute-c"
	got, err := WaitLoad([]Event{replica, older, primary, stopped},
		waitT0.Add(-time.Minute), end.Add(time.Minute))
	if err != nil {
		t.Fatalf("WaitLoad: %v", err)
	}
	if len(got.Points) != 3 {
		t.Fatalf("points = %+v, want the 3 distinct intervals", got.Points)
	}
	if len(got.Current) != 1 || !near(got.Current["IO"], 2.0) {
		t.Fatalf("current = %v, want IO 2.0 (primary 0.5 + replica 1.5), no Lock", got.Current)
	}
	if !got.CurrentAt.Equal(end.Add(5 * time.Second)) {
		t.Fatalf("current at %v, want the newest interval end %v", got.CurrentAt,
			end.Add(5*time.Second))
	}
}

func TestWaitLoad_CurrentLoadEmptyWithoutIntervals(t *testing.T) {
	got, err := WaitLoad(nil, waitT0, waitT0.Add(time.Hour))
	if err != nil || got.Current == nil || len(got.Current) != 0 || !got.CurrentAt.IsZero() {
		t.Fatalf("got %+v err %v, want an empty non-nil current load", got, err)
	}
}
