package rca

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// rca-07: log incidents carry the classifier's counts and PIDs.

var occT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func occSignal(count int, pids []int, first, last time.Time) *Signal {
	return &Signal{ID: signalid.LogDeadlockDetected, FiredAt: last, Severity: "critical",
		Metrics: map[string]any{
			"database": "appdb", "pid": pids[0], "occurrence_count": count,
			"affected_pids": pids,
			"first_seen":    first.Format(time.RFC3339Nano),
			"last_seen":     last.Format(time.RFC3339Nano),
		}}
}

// CHECK-02 companion: the deadlock evidence reads the classifier's
// affected_pids and occurrence_count (it read a "pids" metric nobody set).
func TestLogTreeDeadlock_EvidenceNamesPIDsAndCount(t *testing.T) {
	s := occSignal(3, []int{101, 102, 103}, occT0, occT0.Add(time.Minute))
	inc := logTreeDeadlock(s)
	ev := inc.CausalChain[0].Evidence
	for _, want := range []string{"database=appdb", "pids=101,102,103", "occurrences=3"} {
		if !strings.Contains(ev, want) {
			t.Errorf("evidence %q lacks %q", ev, want)
		}
	}
	s.Metrics["self_inflicted"] = true
	if ev := logTreeDeadlock(s).CausalChain[0].Evidence; !strings.Contains(ev,
		"[self-inflicted deadlock]") {
		t.Errorf("self-inflicted marker lost: %q", ev)
	}
}

// Nil/empty: a signal without counts builds the old evidence and no
// occurrences, never "pids=" with nothing after it.
func TestLogTreeDeadlock_NoCountsNoOccurrences(t *testing.T) {
	s := firedSignal(signalid.LogDeadlockDetected, map[string]any{"database": "appdb"})
	got := newTestEngine().runLogDecisionTrees([]*Signal{s})
	if len(got) != 1 {
		t.Fatalf("incidents = %d, want 1", len(got))
	}
	if got[0].LogOccurrences != nil {
		t.Errorf("LogOccurrences = %+v, want nil", got[0].LogOccurrences)
	}
	if ev := got[0].CausalChain[0].Evidence; ev != "database=appdb" {
		t.Errorf("evidence = %q, want database=appdb", ev)
	}
}

// Happy path: the trees fold every signal of an id in a batch, so an
// immediate signal and its follow-up count the window once.
func TestRunLogDecisionTrees_FoldsImmediateAndFollowUp(t *testing.T) {
	first := occSignal(1, []int{101}, occT0, occT0)
	up := occSignal(3, []int{101, 102, 103}, occT0, occT0.Add(2*time.Second))
	got := newTestEngine().runLogDecisionTrees([]*Signal{first, up})
	if len(got) != 1 {
		t.Fatalf("incidents = %d, want 1", len(got))
	}
	occ := got[0].LogOccurrences
	if occ == nil || occ.Count != 3 || !reflect.DeepEqual(occ.PIDs, []int{101, 102, 103}) {
		t.Fatalf("occurrences = %+v, want count 3 pids 101-103", occ)
	}
	if !occ.FirstSeen.Equal(occT0) || !occ.LastSeen.Equal(occT0.Add(2*time.Second)) {
		t.Errorf("first/last = %v/%v", occ.FirstSeen, occ.LastSeen)
	}
}

// Invalid input: malformed metrics are ignored rather than trusted.
func TestLogOccurrencesFromSignal_InvalidMetrics(t *testing.T) {
	cases := map[string]map[string]any{
		"no count":       {"affected_pids": []int{1}},
		"zero count":     {"occurrence_count": 0},
		"negative count": {"occurrence_count": -4},
		"string count":   {"occurrence_count": "3"},
	}
	for name, m := range cases {
		got := logOccurrencesFromSignal(&Signal{ID: "x", FiredAt: occT0, Metrics: m})
		if got != nil {
			t.Errorf("%s: occurrences = %+v, want nil", name, got)
		}
	}
	if logOccurrencesFromSignal(nil) != nil {
		t.Error("nil signal built occurrences")
	}
	bad := &Signal{ID: "x", FiredAt: occT0, Metrics: map[string]any{
		"occurrence_count": 2, "first_seen": "yesterday", "affected_pids": "1,2"}}
	got := logOccurrencesFromSignal(bad)
	if got == nil || got.Count != 2 || len(got.PIDs) != 0 ||
		!got.FirstSeen.Equal(occT0) || !got.LastSeen.Equal(occT0) {
		t.Errorf("bad times/pids = %+v, want count 2, FiredAt times, no pids", got)
	}
}

// Merge rules: one window counts once, new windows add up, PIDs union to
// at most 20, first is the earliest and last the latest.
func TestMergeLogOccurrences(t *testing.T) {
	w1 := logOccurrencesFromSignal(occSignal(1, []int{101}, occT0, occT0))
	w1b := logOccurrencesFromSignal(
		occSignal(3, []int{101, 102, 103}, occT0, occT0.Add(time.Second)))
	m := mergeLogOccurrences(w1, w1b)
	if m.Count != 3 {
		t.Fatalf("same window count = %d, want 3", m.Count)
	}
	if again := mergeLogOccurrences(m, w1b); again.Count != 3 {
		t.Errorf("re-merging the same follow-up counted it twice: %d", again.Count)
	}
	t2 := occT0.Add(10 * time.Minute)
	w2 := logOccurrencesFromSignal(occSignal(2, []int{104, 101}, t2, t2.Add(time.Second)))
	m2 := mergeLogOccurrences(m, w2)
	if m2.Count != 5 || !m2.FirstSeen.Equal(occT0) || !m2.LastSeen.Equal(t2.Add(time.Second)) {
		t.Errorf("two windows = %+v, want count 5 spanning both", m2)
	}
	if !reflect.DeepEqual(m2.PIDs, []int{101, 102, 103, 104}) {
		t.Errorf("pids = %v, want union [101 102 103 104]", m2.PIDs)
	}
	if m.Count != 3 || len(m.PIDs) != 3 {
		t.Errorf("merge mutated its input: %+v", m)
	}
	if mergeLogOccurrences(nil, nil) != nil || mergeLogOccurrences(m, nil) != m ||
		mergeLogOccurrences(nil, w2).Count != 2 {
		t.Error("nil merge rules broken")
	}
	var many []int
	for i := 0; i < 30; i++ {
		many = append(many, 1000+i)
	}
	t3 := occT0.Add(time.Hour)
	big := mergeLogOccurrences(m2, logOccurrencesFromSignal(occSignal(30, many, t3, t3)))
	if len(big.PIDs) != maxLogOccurrencePIDs || maxLogOccurrencePIDs != 20 {
		t.Errorf("pids = %d, want capped at 20", len(big.PIDs))
	}
}

// State transition: merging into an open incident refreshes occurrences.
func TestEngineMerge_RefreshesLogOccurrences(t *testing.T) {
	eng := newTestEngine()
	first := newTestEngine().runLogDecisionTrees([]*Signal{
		occSignal(1, []int{101}, occT0, occT0)})[0]
	eng.mu.Lock()
	eng.dedup(&first)
	later := occT0.Add(5 * time.Minute)
	up := newTestEngine().runLogDecisionTrees([]*Signal{
		occSignal(4, []int{101, 102}, occT0, later)})[0]
	eng.dedup(&up)
	open := eng.incidents
	eng.mu.Unlock()
	if len(open) != 1 {
		t.Fatalf("incidents = %d, want 1", len(open))
	}
	occ := open[0].LogOccurrences
	if occ == nil || occ.Count != 4 || !occ.LastSeen.Equal(later) {
		t.Fatalf("occurrences = %+v, want count 4 last %v", occ, later)
	}
}

// quietEngine returns an engine on a fake clock with one open deadlock
// incident last seen at occT0 and the grace period spent.
func quietEngine(t *testing.T) (*Engine, *time.Time) {
	t.Helper()
	eng := newTestEngine()
	now := occT0
	eng.now = func() time.Time { return now }
	eng.gracePeriodLeft = 0
	inc := newTestEngine().runLogDecisionTrees([]*Signal{
		occSignal(1, []int{101}, occT0, occT0)})[0]
	eng.mu.Lock()
	eng.dedup(&inc)
	eng.mu.Unlock()
	return eng, &now
}

func openCount(eng *Engine) int {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	return eng.activeCount()
}

// State transition + boundary: a log incident ignores clear cycles and
// resolves once rca.log_quiet_minutes (default: the 30 min dedup window)
// passed since it was last seen.
func TestAutoResolve_LogIncidentWaitsQuietPeriod(t *testing.T) {
	eng, now := quietEngine(t)
	for i := 1; i <= 29; i++ { // 29 clear one-minute cycles
		*now = occT0.Add(time.Duration(i) * time.Minute)
		eng.mu.Lock()
		eng.autoResolve(map[string]bool{})
		eng.mu.Unlock()
	}
	if n := openCount(eng); n != 1 {
		t.Fatalf("open after 29 quiet minutes = %d, want 1", n)
	}
	*now = occT0.Add(30*time.Minute - time.Nanosecond)
	eng.mu.Lock()
	eng.autoResolve(map[string]bool{})
	eng.mu.Unlock()
	if n := openCount(eng); n != 1 {
		t.Fatalf("resolved before the quiet period ended")
	}
	*now = occT0.Add(30 * time.Minute)
	eng.mu.Lock()
	eng.autoResolve(map[string]bool{})
	inc := eng.incidents[0]
	eng.mu.Unlock()
	if inc.ResolvedAt == nil || inc.ResolvedBy != ResolvedByAuto {
		t.Fatalf("not auto-resolved at the quiet boundary: %+v", inc)
	}
	if !inc.ResolvedAt.Equal(*now) {
		t.Errorf("resolved_at = %v, want the engine clock %v", inc.ResolvedAt, *now)
	}
	if !strings.Contains(inc.ResolutionReason, "30 minutes") {
		t.Errorf("reason = %q, want the quiet period named", inc.ResolutionReason)
	}
}

// Config override: rca.log_quiet_minutes shortens the quiet period.
func TestAutoResolve_LogQuietMinutesOverride(t *testing.T) {
	eng, now := quietEngine(t)
	eng.cfg.LogQuietMinutes = 10
	*now = occT0.Add(10 * time.Minute)
	eng.mu.Lock()
	eng.autoResolve(map[string]bool{})
	eng.mu.Unlock()
	if n := openCount(eng); n != 0 {
		t.Fatalf("open after the 10 min override = %d, want 0", n)
	}
}

// Must not change: a metric-source incident still resolves after
// resolution_cycles clear cycles, whatever the clock says.
func TestAutoResolve_MetricIncidentStillCountsCycles(t *testing.T) {
	eng := newTestEngine()
	eng.now = func() time.Time { return occT0 }
	eng.gracePeriodLeft = 0
	inc := Incident{DetectedAt: occT0, Severity: "warning", Source: "deterministic",
		SignalIDs: []string{signalid.ConnectionsHigh}, RootCause: "conn"}
	eng.mu.Lock()
	eng.dedup(&inc)
	eng.autoResolve(map[string]bool{})
	eng.autoResolve(map[string]bool{})
	eng.mu.Unlock()
	if n := openCount(eng); n != 0 {
		t.Fatalf("metric incident open after 2 clear cycles = %d, want 0", n)
	}
}

// suppressBase is a critical log recurrence detected 2 h after its
// critical predecessor resolved.
func suppressBase() (Incident, persistResult) {
	resolved := occT0
	return Incident{ID: "new", Source: "log_deterministic", Severity: "critical",
			DetectedAt: occT0.Add(2 * time.Hour)},
		persistResult{inserted: true, previousID: "old",
			prevResolvedAt: &resolved, prevSeverity: "critical"}
}

func TestSuppressRecurrence(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*Incident, *persistResult)
		want bool
	}{
		{"recurrence 2h after resolution", func(*Incident, *persistResult) {}, true},
		{"just inside 6h", func(i *Incident, _ *persistResult) {
			i.DetectedAt = occT0.Add(6*time.Hour - time.Second)
		}, true},
		{"exactly 6h notifies", func(i *Incident, _ *persistResult) {
			i.DetectedAt = occT0.Add(6 * time.Hour)
		}, false},
		{"7h notifies", func(i *Incident, _ *persistResult) {
			i.DetectedAt = occT0.Add(7 * time.Hour)
		}, false},
		{"severity rise notifies", func(_ *Incident, r *persistResult) {
			r.prevSeverity = "warning"
		}, false},
		{"lower severity is suppressed", func(i *Incident, _ *persistResult) {
			i.Severity = "warning"
		}, true},
		{"metric incident notifies", func(i *Incident, _ *persistResult) {
			i.Source = "deterministic"
		}, false},
		{"no predecessor notifies", func(_ *Incident, r *persistResult) {
			r.previousID = ""
		}, false},
		{"open predecessor notifies", func(_ *Incident, r *persistResult) {
			r.prevResolvedAt = nil
		}, false},
	}
	eng := newTestEngine()
	for _, tc := range tests {
		inc, r := suppressBase()
		tc.mod(&inc, &r)
		if got := eng.suppressRecurrence(&inc, r); got != tc.want {
			t.Errorf("%s: suppress = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Config override: a shorter rca.recurrence_renotify_hours pages sooner.
func TestSuppressRecurrence_RenotifyHoursOverride(t *testing.T) {
	eng := newTestEngine()
	eng.cfg.RecurrenceRenotifyHours = 1
	inc, r := suppressBase()
	if eng.suppressRecurrence(&inc, r) {
		t.Error("rca.recurrence_renotify_hours = 1 still suppressed a 2h recurrence")
	}
}

// A suppressed recurrence sends no incident_detected, logs a DEBUG line
// naming it, and its later resolution is not notified either.
func TestApplyOne_SuppressedRecurrenceIsSilent(t *testing.T) {
	var lines []string
	eng := NewEngine(testRCACfg(), func(level, msg string, args ...any) {
		lines = append(lines, level+" "+fmt.Sprintf(msg, args...))
	})
	resolved := occT0
	inc := Incident{ID: "rec-1", Source: "log_deterministic", Severity: "critical",
		DetectedAt: occT0.Add(time.Hour), RootCause: "Deadlock"}
	eng.trackFor(inc.ID).notifyDetected = true
	evs := eng.applyOne(&inc, persistResult{id: inc.ID, inserted: true,
		previousID: "prev-1", prevResolvedAt: &resolved, prevSeverity: "critical"})
	if len(evs) != 0 {
		t.Fatalf("events = %d, want none for a suppressed recurrence", len(evs))
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "debug ") ||
		!strings.Contains(lines[0], "rec-1") || !strings.Contains(lines[0], "prev-1") {
		t.Errorf("log lines = %q, want one debug line naming both incidents", lines)
	}
	now := occT0.Add(2 * time.Hour)
	inc.ResolvedAt = &now
	if evs := eng.applyOne(&inc, persistResult{id: inc.ID, resolved: true}); len(evs) != 0 {
		t.Errorf("resolution of a silent recurrence sent %d events", len(evs))
	}
}
