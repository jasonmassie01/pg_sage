package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// Detector episodes create incidents: with an episode sink (the RCA
// engine in production) the first poll of an episode records an incident
// and every trigger of that episode links to it exactly like an RCA
// incident trigger (case id, incident id, subject, idempotency key), so
// the Cases panel, notifications and M7 see detector episodes. Later polls
// of the same episode only refresh that incident; a new episode asks
// again (the sink attaches it to an open incident or opens a new one).

// fakeSink records episodes and answers with scripted incidents.
type fakeSink struct {
	mu       sync.Mutex
	episodes []Episode
	next     int
	// refuse makes the sink return no incident (cap reached, store down).
	refuse bool
	err    error
	// resolved makes continuing observations report no open incident.
	resolved bool
}

func (s *fakeSink) RecordEpisode(_ context.Context, ep Episode) (EpisodeIncident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.episodes = append(s.episodes, ep)
	if s.refuse {
		return EpisodeIncident{}, s.err
	}
	if ep.IncidentID != "" {
		if s.resolved {
			return EpisodeIncident{}, s.err
		}
		return s.incident(ep, ep.IncidentID), s.err
	}
	s.next++
	return s.incident(ep, fmt.Sprintf("00000000-0000-4000-8000-%012d", s.next)), s.err
}

func (s *fakeSink) incident(ep Episode, id string) EpisodeIncident {
	return EpisodeIncident{ID: id, Database: ep.Database, SignalIDs: []string{ep.Signal},
		Source: "deterministic"}
}

func (s *fakeSink) all() []Episode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Episode(nil), s.episodes...)
}

func linkedTrigger(id, signal string, kind TriggerKind) Trigger {
	caseID := clip(cases.IncidentIdentityKey(cases.SourceIncident{ID: id,
		DatabaseName: "orders", SignalIDs: []string{signal}, Source: "deterministic"}))
	return Trigger{CaseID: caseID, IncidentID: id, Kind: kind, Subject: "incident " + id,
		IdempotencyKey: "incident:" + id}
}

func stormRunner() *seqRunner {
	return newSeqRunner().add(ckptPoll(0, 50, 10), ckptPoll(time.Minute, 51, 14),
		ckptPoll(2*time.Minute, 51, 16), ckptPoll(3*time.Minute, 51, 18))
}

func TestDetectorIncident_EpisodeOpensOneIncidentAndLinksEveryTrigger(t *testing.T) {
	sink := &fakeSink{}
	d, _ := newDetector(t, stormRunner())
	d.WithIncidents(sink)
	poll(t, d)
	first := poll(t, d)[TriggerCheckpoint]
	eps := sink.all()
	if len(eps) != 1 {
		t.Fatalf("sink saw %d episodes, want 1", len(eps))
	}
	id := "00000000-0000-4000-8000-000000000001"
	want := linkedTrigger(id, "sre_checkpoint_storm", TriggerCheckpoint)
	if first != want {
		t.Fatalf("trigger = %+v\nwant      %+v", first, want)
	}
	ep := eps[0]
	if ep.Kind != TriggerCheckpoint || ep.Database != "orders" ||
		ep.Signal != "sre_checkpoint_storm" || ep.IncidentID != "" ||
		ep.Severity != "warning" || !ep.Start.Equal(dt0.Add(time.Minute)) ||
		!ep.ObservedAt.Equal(dt0.Add(time.Minute)) {
		t.Fatalf("first episode = %+v", ep)
	}
	for i := 0; i < 2; i++ {
		if again := poll(t, d)[TriggerCheckpoint]; again != want {
			t.Fatalf("poll %d of the same episode: %+v, want %+v", i+3, again, want)
		}
	}
	eps = sink.all()
	if len(eps) != 3 || eps[1].IncidentID != id || eps[2].IncidentID != id ||
		!eps[2].Start.Equal(dt0.Add(time.Minute)) ||
		!eps[2].ObservedAt.Equal(dt0.Add(3*time.Minute)) {
		t.Fatalf("continuing observations = %+v, want refreshes of %s", eps, id)
	}
}

// The incident carries the family's measured evidence against its
// threshold, and the RCA signals of the same family it may attach to.
func TestDetectorIncident_EvidencePerFamily(t *testing.T) {
	r := stormRunner().add(tempPoll(0, 1<<30), tempPoll(time.Minute, 1<<30+1536<<20)).
		add(lwPoll(0, "WALWrite", 9), lwPoll(time.Second, "WALWrite", 11),
			lwPoll(2*time.Second, "WALWrite", 12))
	sink := &fakeSink{}
	d, _ := newDetector(t, r)
	d.WithIncidents(sink)
	poll(t, d)
	poll(t, d)
	poll(t, d)
	byKind := map[TriggerKind]Episode{}
	for _, ep := range sink.all() {
		if _, seen := byKind[ep.Kind]; !seen {
			byKind[ep.Kind] = ep
		}
	}
	checks := []struct {
		kind    TriggerKind
		signal  string
		related []string
		facts   []string
	}{
		{TriggerCheckpoint, "sre_checkpoint_storm", []string{"log_checkpoint_too_frequent"},
			[]string{"4 requested", "1 timed", "5m0s", "threshold 3"}},
		{TriggerTempFiles, "sre_temp_file_explosion", []string{"log_temp_file_created"},
			[]string{"1536 MiB", "5m0s", "threshold 1024 MiB"}},
		{TriggerLWLock, "sre_lwlock_contention", nil,
			[]string{"12 backends", "WALWrite", "3 consecutive polls", "threshold 8"}},
	}
	for _, c := range checks {
		ep, ok := byKind[c.kind]
		if !ok {
			t.Errorf("%s: no episode recorded", c.kind)
			continue
		}
		if ep.Signal != c.signal || ep.Severity != "warning" || ep.Summary == "" ||
			strings.Join(ep.Related, ",") != strings.Join(c.related, ",") {
			t.Errorf("%s episode = %+v", c.kind, ep)
		}
		for _, f := range c.facts {
			if !strings.Contains(ep.Evidence, f) {
				t.Errorf("%s evidence %q lacks %q", c.kind, ep.Evidence, f)
			}
		}
	}
}

// After the cooldown a new episode asks the sink again without an
// incident: the sink decides whether it attaches to an open incident.
func TestDetectorIncident_NewEpisodeAfterCooldownAsksAgain(t *testing.T) {
	r := newSeqRunner().add(ckptPoll(0, 0, 0), ckptPoll(15*time.Second, 0, 3),
		ckptPoll(90*time.Second, 0, 3), ckptPoll(32*time.Minute, 0, 3),
		ckptPoll(32*time.Minute+15*time.Second, 0, 6))
	sink := &fakeSink{}
	d, _ := newDetector(t, r, func(c *DetectorConfig) { c.Window = time.Minute })
	d.WithIncidents(sink)
	poll(t, d)
	first := poll(t, d)[TriggerCheckpoint]
	poll(t, d)
	poll(t, d)
	second := poll(t, d)[TriggerCheckpoint]
	eps := sink.all()
	if len(eps) != 2 || eps[0].IncidentID != "" || eps[1].IncidentID != "" ||
		!eps[1].Start.Equal(dt0.Add(32*time.Minute+15*time.Second)) {
		t.Fatalf("episodes = %+v, want two first observations", eps)
	}
	if first.IncidentID == "" || second.IncidentID == "" ||
		first.IncidentID == second.IncidentID {
		t.Fatalf("triggers %+v / %+v, want each episode linked to its incident", first,
			second)
	}
}

// Inside the cooldown a recurrence neither fires nor reaches the sink.
func TestDetectorIncident_CooldownSuppressesTheSink(t *testing.T) {
	r := newSeqRunner().add(ckptPoll(0, 0, 0), ckptPoll(15*time.Second, 0, 3),
		ckptPoll(90*time.Second, 0, 3), ckptPoll(100*time.Second, 0, 6))
	sink := &fakeSink{}
	d, _ := newDetector(t, r, func(c *DetectorConfig) { c.Window = time.Minute })
	d.WithIncidents(sink)
	for i := 0; i < 4; i++ {
		poll(t, d)
	}
	if n := len(sink.all()); n != 1 {
		t.Fatalf("sink saw %d observations, want 1 (the recurrence is in the cooldown)", n)
	}
}

// A sink that cannot record the incident never blocks detection: the
// legacy detector trigger starts the investigation, the failure is
// logged once, and the next poll retries the first observation.
func TestDetectorIncident_SinkFailureFallsBackAndRetries(t *testing.T) {
	sink := &fakeSink{refuse: true, err: errors.New("incident store unreachable")}
	d, logs := newDetector(t, stormRunner())
	d.WithIncidents(sink)
	poll(t, d)
	tr := poll(t, d)[TriggerCheckpoint]
	if tr.IncidentID != "" || tr.CaseID != "sre:detector:checkpoint_storm:orders" ||
		!strings.HasPrefix(tr.IdempotencyKey, "detector:checkpoint_storm:") {
		t.Fatalf("fallback trigger = %+v", tr)
	}
	poll(t, d)
	if n := logs.count("incident store unreachable"); n != 1 {
		t.Fatalf("sink failure logged %d times, want once: %v", n, logs.lines)
	}
	sink.mu.Lock()
	sink.refuse, sink.err = false, nil
	sink.mu.Unlock()
	linked := poll(t, d)[TriggerCheckpoint]
	eps := sink.all()
	if linked.IncidentID == "" || eps[len(eps)-1].IncidentID != "" {
		t.Fatalf("after recovery: trigger %+v, last episode %+v", linked, eps[len(eps)-1])
	}
}

// A store failure after the engine tracked the incident (the row is
// persisted later) still links: the incident id is stable.
func TestDetectorIncident_PersistFailureStillLinks(t *testing.T) {
	sink := &fakeSink{err: errors.New("persist failed")}
	d, logs := newDetector(t, stormRunner())
	d.WithIncidents(sink)
	poll(t, d)
	tr := poll(t, d)[TriggerCheckpoint]
	if tr.IncidentID != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("trigger = %+v, want it linked despite the persist error", tr)
	}
	if logs.count("persist failed") != 1 {
		t.Fatalf("persist failure not logged once: %v", logs.lines)
	}
}

// An incident resolved during its episode (an operator resolved it) is
// not reopened: the episode keeps its link and no new incident is asked
// for until the next episode.
func TestDetectorIncident_ResolvedIncidentKeepsTheEpisodeLink(t *testing.T) {
	sink := &fakeSink{}
	d, _ := newDetector(t, stormRunner())
	d.WithIncidents(sink)
	poll(t, d)
	first := poll(t, d)[TriggerCheckpoint]
	sink.mu.Lock()
	sink.resolved = true
	sink.mu.Unlock()
	again := poll(t, d)[TriggerCheckpoint]
	if again != first {
		t.Fatalf("after resolution: %+v, want the episode's link %+v", again, first)
	}
	for _, ep := range sink.all()[1:] {
		if ep.IncidentID != first.IncidentID {
			t.Fatalf("continuing observation asked for a new incident: %+v", ep)
		}
	}
}

// Concurrent polls are serialized: one first observation per episode.
func TestDetectorIncident_ConcurrentPollsAskOnce(t *testing.T) {
	r := newSeqRunner()
	for i := 0; i < 40; i++ {
		r.add(ckptPoll(time.Duration(i)*time.Second, 0, int64(3*i)))
	}
	sink := &fakeSink{}
	d, _ := newDetector(t, r)
	d.WithIncidents(sink)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := d.Triggers(context.Background()); err != nil {
					t.Errorf("Triggers: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	firsts := 0
	for _, ep := range sink.all() {
		if ep.IncidentID == "" {
			firsts++
		}
	}
	if firsts != 1 {
		t.Fatalf("%d first observations for one episode, want 1", firsts)
	}
}

// Detector incidents map back to their families, so the RCA trigger
// source resumes them after a restart under the same idempotency key.
func TestDetectorSignals_MapToTheirFamilies(t *testing.T) {
	for kind, signal := range map[TriggerKind]string{
		TriggerCheckpoint: "sre_checkpoint_storm",
		TriggerTempFiles:  "sre_temp_file_explosion",
		TriggerLWLock:     "sre_lwlock_contention",
	} {
		if got := DetectorSignal(kind); got != signal {
			t.Errorf("DetectorSignal(%s) = %q, want %q", kind, got, signal)
		}
		if got, ok := incidentKind([]string{signal}); !ok || got != kind {
			t.Errorf("incidentKind(%s) = %s %v, want %s", signal, got, ok, kind)
		}
	}
	if got := DetectorSignal(TriggerLock); got != "" {
		t.Errorf("DetectorSignal(lock_blocking) = %q, want none", got)
	}
}
