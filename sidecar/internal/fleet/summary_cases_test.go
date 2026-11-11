package fleet

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// cases-11: the fleet summary carries what the Overview hero needs to
// refuse a health claim: warning findings, open incidents, incident counts
// that could not be read, and collection freshness.

func TestCollectionFreshness_Boundary(t *testing.T) {
	limit := staleIntervals * healthInterval
	registered := ago(time.Hour)
	cases := []struct {
		name    string
		lastRun time.Time
		want    string
	}{
		{"just sampled", ago(time.Second), FreshnessFresh},
		{"exactly 3 intervals old", ago(limit), FreshnessFresh},
		{"3 intervals and 1s old", ago(limit + time.Second), FreshnessStale},
		{"future sample", healthNow.Add(time.Minute), FreshnessFresh},
		{"never sampled, registered long ago", time.Time{}, FreshnessStale},
	}
	for _, tc := range cases {
		got := collectionFreshness(tc.lastRun, healthInterval, registered, healthNow)
		if got != tc.want {
			t.Errorf("%s: freshness %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCollectionFreshness_UnknownWithoutEvidence(t *testing.T) {
	// A database in its first collection window has no sample yet: that
	// is unknown, not fresh.
	if got := collectionFreshness(time.Time{}, healthInterval, ago(time.Second),
		healthNow); got != FreshnessUnknown {
		t.Errorf("new never-sampled: %q, want unknown", got)
	}
	if got := collectionFreshness(time.Time{}, healthInterval, time.Time{},
		healthNow); got != FreshnessUnknown {
		t.Errorf("unknown registration: %q, want unknown", got)
	}
	// No interval to judge against: never claim fresh.
	if got := collectionFreshness(ago(time.Second), 0, ago(time.Hour),
		healthNow); got != FreshnessUnknown {
		t.Errorf("zero interval: %q, want unknown", got)
	}
}

func TestCollectionFreshness_AgreesWithStaleReason(t *testing.T) {
	limit := staleIntervals * healthInterval
	runs := []time.Time{{}, ago(time.Second), ago(limit), ago(limit + time.Second)}
	for _, reg := range []time.Time{{}, ago(time.Second), ago(time.Hour)} {
		for _, run := range runs {
			stale := staleReason(run, healthInterval, reg, healthNow) != ""
			got := collectionFreshness(run, healthInterval, reg, healthNow)
			if stale != (got == FreshnessStale) {
				t.Errorf("run %v reg %v: stale reason %v, freshness %q",
					run, reg, stale, got)
			}
		}
	}
}

func TestSummary_CaseCountsSumAcrossDatabases(t *testing.T) {
	mgr := healthManager()
	a := freshStatus()
	a.FindingsWarning, a.FindingsCritical = 2, 1
	a.IncidentsCritical, a.IncidentsWarning = 1, 0
	b := freshStatus()
	b.FindingsWarning = 3
	b.IncidentsCritical, b.IncidentsWarning = 2, 4
	registerHealthDB(mgr, "a", a)
	registerHealthDB(mgr, "b", b)
	s := mgr.FleetStatus().Summary
	if s.TotalWarning != 5 || s.TotalCritical != 1 {
		t.Errorf("findings: warning %d critical %d, want 5 and 1",
			s.TotalWarning, s.TotalCritical)
	}
	if s.IncidentsCritical != 3 || s.IncidentsWarning != 4 || s.IncidentsUnknown != 0 {
		t.Errorf("incidents: critical %d warning %d unknown %d, want 3, 4, 0",
			s.IncidentsCritical, s.IncidentsWarning, s.IncidentsUnknown)
	}
	if s.StaleCollection != 0 || s.FreshnessUnknown != 0 {
		t.Errorf("freshness: stale %d unknown %d, want 0 and 0",
			s.StaleCollection, s.FreshnessUnknown)
	}
}

func TestSummary_UnknownIncidentsAreNeverZero(t *testing.T) {
	mgr := healthManager()
	unread := freshStatus()
	unread.IncidentsCritical, unread.IncidentsWarning = IncidentsUnknown, IncidentsUnknown
	known := freshStatus()
	known.IncidentsCritical = 1
	registerHealthDB(mgr, "unread", unread)
	registerHealthDB(mgr, "known", known)
	s := mgr.FleetStatus().Summary
	if s.IncidentsUnknown != 1 {
		t.Errorf("incidents_unknown %d, want 1", s.IncidentsUnknown)
	}
	// The -1 marker must not subtract from the known database's count.
	if s.IncidentsCritical != 1 || s.IncidentsWarning != 0 {
		t.Errorf("incidents critical %d warning %d, want 1 and 0",
			s.IncidentsCritical, s.IncidentsWarning)
	}
}

func TestSummary_StaleAndUnknownCollection(t *testing.T) {
	mgr := healthManager()
	stale := freshStatus()
	stale.CollectorLastRun = time.Now().Add(-time.Hour)
	registerHealthDB(mgr, "stale", stale)
	never := freshStatus()
	never.CollectorLastRun = time.Time{}
	mgr.RegisterInstance(&DatabaseInstance{Name: "never-old",
		RegisteredAt: time.Now().Add(-time.Hour), Status: &never})
	newDB := freshStatus()
	newDB.CollectorLastRun = time.Time{}
	registerHealthDB(mgr, "never-new", newDB)
	registerHealthDB(mgr, "fresh", freshStatus())
	o := mgr.FleetStatus()
	if o.Summary.StaleCollection != 2 || o.Summary.FreshnessUnknown != 1 {
		t.Fatalf("stale %d unknown %d, want 2 and 1",
			o.Summary.StaleCollection, o.Summary.FreshnessUnknown)
	}
	want := map[string]string{"stale": FreshnessStale, "never-old": FreshnessStale,
		"never-new": FreshnessUnknown, "fresh": FreshnessFresh}
	for name, freshness := range want {
		if got := rowByName(t, o, name).Status.CollectionFreshness; got != freshness {
			t.Errorf("%s: collection_freshness %q, want %q", name, got, freshness)
		}
	}
}

func TestSummary_ZeroIntervalIsFreshnessUnknown(t *testing.T) {
	mgr := newTestManager("a") // no collector interval configured
	mgr.GetInstance("a").Status.CollectorLastRun = time.Now()
	s := mgr.FleetStatus().Summary
	if s.FreshnessUnknown != 1 || s.StaleCollection != 0 {
		t.Errorf("unknown %d stale %d, want 1 and 0", s.FreshnessUnknown, s.StaleCollection)
	}
}

func TestSummary_EmptyFleetIsAllZero(t *testing.T) {
	s := healthManager().FleetStatus().Summary
	if s != (FleetSummary{}) {
		t.Errorf("empty fleet summary %+v, want zero", s)
	}
}

func TestSummary_JSONCarriesCaseFields(t *testing.T) {
	mgr := healthManager()
	st := freshStatus()
	st.FindingsWarning, st.IncidentsCritical, st.IncidentsWarning = 1, 2, 3
	registerHealthDB(mgr, "a", st)
	raw, err := json.Marshal(mgr.FleetStatus())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Summary   map[string]any `json:"summary"`
		Databases []struct {
			Status map[string]any `json:"status"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantSummary := map[string]float64{"total_warning": 1, "incidents_critical": 2,
		"incidents_warning": 3, "incidents_unknown": 0, "stale_collection": 0,
		"freshness_unknown": 0, "total_critical": 0, "degraded": 0}
	for key, want := range wantSummary {
		if v, ok := got.Summary[key].(float64); !ok || v != want {
			t.Errorf("summary.%s = %v, want %v", key, got.Summary[key], want)
		}
	}
	status := got.Databases[0].Status
	if status["incidents_critical"] != 2.0 || status["incidents_warning"] != 3.0 ||
		status["collection_freshness"] != FreshnessFresh {
		t.Errorf("status case fields: %v %v %v", status["incidents_critical"],
			status["incidents_warning"], status["collection_freshness"])
	}
}

func TestSummary_ConcurrentIncidentUpdates(t *testing.T) {
	mgr := healthManager()
	inst := registerHealthDB(mgr, "a", freshStatus())
	inst.UpdateStatus(func(s *InstanceStatus) { s.IncidentsCritical = 1 })
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			n := 1 + i%2
			inst.UpdateStatus(func(s *InstanceStatus) { s.IncidentsCritical = n })
		}
	}()
	for i := 0; i < 200; i++ {
		if got := mgr.FleetStatus().Summary.IncidentsCritical; got != 1 && got != 2 {
			close(stop)
			wg.Wait()
			t.Fatalf("summary read a torn count %d", got)
		}
	}
	close(stop)
	wg.Wait()
}
