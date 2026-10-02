package rca

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sage SRE M6 detector episodes become incidents through the engine, the
// single owner of incident state: the first observation of an episode
// opens an incident (or attaches to an open one), later observations of
// the same episode refresh it and keep it from auto-resolving, a new
// episode counts an occurrence on the open incident, and an incident
// resolved during its episode is never reopened by that episode.

var ep0 = time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC)

func checkpointEpisode(at time.Duration) Episode {
	return Episode{Signal: "sre_checkpoint_storm",
		Related: []string{"log_checkpoint_too_frequent"}, Severity: "warning",
		RootCause:  "Checkpoint storm: requested checkpoints outnumber timed ones",
		Evidence:   "4 requested and 1 timed checkpoints within 5m0s (threshold 3)",
		ObservedAt: ep0.Add(at)}
}

func episodeEngine(t *testing.T) *Engine {
	t.Helper()
	e := testEngine()
	e.WithDatabaseName("orders")
	return e
}

func openWithSignal(e *Engine, signal string) []Incident {
	var out []Incident
	for _, inc := range e.ActiveIncidents() {
		for _, s := range inc.SignalIDs {
			if s == signal {
				out = append(out, inc)
			}
		}
	}
	return out
}

func TestObserveEpisode_FirstObservationOpensAnIncident(t *testing.T) {
	e := episodeEngine(t)
	inc, ok := e.ObserveEpisode(context.Background(), checkpointEpisode(0))
	if !ok || inc.ID == "" {
		t.Fatalf("ObserveEpisode = %+v, %v; want a new incident", inc, ok)
	}
	if inc.Severity != "warning" || inc.Source != "deterministic" ||
		inc.DatabaseName != "orders" || inc.OccurrenceCount != 1 ||
		strings.Join(inc.SignalIDs, ",") != "sre_checkpoint_storm" ||
		!strings.HasPrefix(inc.RootCause, "Checkpoint storm") ||
		!inc.DetectedAt.Equal(ep0) || inc.ResolvedAt != nil {
		t.Fatalf("incident = %+v", inc)
	}
	if len(inc.CausalChain) != 1 || inc.CausalChain[0].Signal != "sre_checkpoint_storm" ||
		!strings.Contains(inc.CausalChain[0].Evidence, "4 requested") {
		t.Fatalf("causal chain = %+v, want the measured evidence", inc.CausalChain)
	}
	if got := openWithSignal(e, "sre_checkpoint_storm"); len(got) != 1 || got[0].ID != inc.ID {
		t.Fatalf("open incidents = %+v", got)
	}
	e.mu.Lock()
	pending := e.trackFor(inc.ID).notifyDetected
	e.mu.Unlock()
	if !pending {
		t.Fatal("the new incident has no pending incident_detected notification")
	}
}

func TestObserveEpisode_ContinuingObservationRefreshesWithoutCounting(t *testing.T) {
	e := episodeEngine(t)
	first, _ := e.ObserveEpisode(context.Background(), checkpointEpisode(0))
	next := checkpointEpisode(45 * time.Second)
	next.IncidentID = first.ID
	again, ok := e.ObserveEpisode(context.Background(), next)
	if !ok || again.ID != first.ID || again.OccurrenceCount != 1 ||
		!again.LastDetectedAt.Equal(ep0.Add(45*time.Second)) {
		t.Fatalf("refresh = %+v, %v; want %s refreshed, one occurrence", again, ok, first.ID)
	}
	if n := len(openWithSignal(e, "sre_checkpoint_storm")); n != 1 {
		t.Fatalf("%d open detector incidents, want 1", n)
	}
}

// A new episode while the previous episode's incident is still open
// attaches to it and counts one occurrence (dedup per episode).
func TestObserveEpisode_NewEpisodeAttachesToTheOpenIncident(t *testing.T) {
	e := episodeEngine(t)
	first, _ := e.ObserveEpisode(context.Background(), checkpointEpisode(0))
	second, ok := e.ObserveEpisode(context.Background(), checkpointEpisode(20*time.Minute))
	if !ok || second.ID != first.ID || second.OccurrenceCount != 2 {
		t.Fatalf("second episode = %+v, %v; want %s with 2 occurrences", second, ok,
			first.ID)
	}
}

// Past the dedup window the open incident is superseded and the new
// episode's incident is linked to it as a recurrence.
func TestObserveEpisode_StaleIncidentIsSuperseded(t *testing.T) {
	e := episodeEngine(t)
	first, _ := e.ObserveEpisode(context.Background(), checkpointEpisode(0))
	later, ok := e.ObserveEpisode(context.Background(), checkpointEpisode(31*time.Minute))
	if !ok || later.ID == first.ID || later.PreviousIncidentID != first.ID ||
		later.OccurrenceCount != 1 {
		t.Fatalf("recurrence = %+v, %v; want a new incident after %s", later, ok, first.ID)
	}
}

// An open RCA incident of the same family that already carries the
// episode's severity is the episode's incident: no duplicate case, and
// the RCA incident is not rewritten.
func TestObserveEpisode_AttachesToARelatedIncidentOfAtLeastItsSeverity(t *testing.T) {
	e := episodeEngine(t)
	logInc := buildLogIncident(ep0, "warning", []string{"log_checkpoint_too_frequent"},
		"Checkpoints occurring too frequently", nil, nil, "", "safe")
	logInc.DatabaseName = "orders"
	e.mu.Lock()
	e.dedup(&logInc)
	e.mu.Unlock()
	before := e.ActiveIncidents()[0]
	got, ok := e.ObserveEpisode(context.Background(), checkpointEpisode(time.Minute))
	if !ok || got.ID != before.ID {
		t.Fatalf("episode = %+v, %v; want the open log incident %s", got, ok, before.ID)
	}
	after := e.ActiveIncidents()
	if len(after) != 1 || after[0].Severity != before.Severity ||
		!after[0].LastDetectedAt.Equal(before.LastDetectedAt) ||
		after[0].OccurrenceCount != before.OccurrenceCount {
		t.Fatalf("incidents after attach = %+v, want the log incident untouched", after)
	}
}

// A lower-severity related incident (temp-file spills are info) would
// hide the episode: the episode opens its own incident.
func TestObserveEpisode_DoesNotAttachBelowItsSeverity(t *testing.T) {
	e := episodeEngine(t)
	logInc := buildLogIncident(ep0, "info", []string{"log_temp_file_created"},
		"Query sort/hash spilling to disk", nil, nil, "", "safe")
	logInc.DatabaseName = "orders"
	e.mu.Lock()
	e.dedup(&logInc)
	e.mu.Unlock()
	got, ok := e.ObserveEpisode(context.Background(), Episode{
		Signal: "sre_temp_file_explosion", Related: []string{"log_temp_file_created"},
		Severity: "warning", RootCause: "Temp-file explosion", Evidence: "1536 MiB",
		ObservedAt: ep0.Add(time.Minute)})
	if !ok || strings.Join(got.SignalIDs, ",") != "sre_temp_file_explosion" ||
		got.Severity != "warning" {
		t.Fatalf("episode = %+v, %v; want its own warning incident", got, ok)
	}
	if n := len(e.ActiveIncidents()); n != 2 {
		t.Fatalf("%d open incidents, want the info spill and the explosion", n)
	}
}

// An incident resolved during its episode is not reopened by that
// episode; the next episode opens a new, linked incident.
func TestObserveEpisode_ResolvedIncidentIsNotReopenedByItsEpisode(t *testing.T) {
	e := episodeEngine(t)
	first, _ := e.ObserveEpisode(context.Background(), checkpointEpisode(0))
	e.mu.Lock()
	e.resolveInMemory(&e.incidents[0], "user:1:ops@example.com", "handled")
	e.mu.Unlock()
	next := checkpointEpisode(time.Minute)
	next.IncidentID = first.ID
	if got, ok := e.ObserveEpisode(context.Background(), next); ok {
		t.Fatalf("a continuing observation reopened %+v", got)
	}
	if n := len(openWithSignal(e, "sre_checkpoint_storm")); n != 0 {
		t.Fatalf("%d open detector incidents after the resolution, want 0", n)
	}
	gone := checkpointEpisode(2 * time.Minute)
	gone.IncidentID = "4b3c2d1e-0000-4000-8000-000000000000"
	if got, ok := e.ObserveEpisode(context.Background(), gone); ok {
		t.Fatalf("an unknown incident id was refreshed: %+v", got)
	}
	recur, ok := e.ObserveEpisode(context.Background(), checkpointEpisode(3*time.Minute))
	if !ok || recur.ID == first.ID {
		t.Fatalf("next episode = %+v, %v; want a new incident", recur, ok)
	}
}

// While the episode is observed the incident stays open; once the
// observations stop, resolution_cycles analyzer cycles resolve it.
func TestObserveEpisode_KeepsTheIncidentOpenWhileObserved(t *testing.T) {
	e := episodeEngine(t)
	e.gracePeriodLeft = 0
	inc, _ := e.ObserveEpisode(context.Background(), checkpointEpisode(0))
	for i := 0; i < 3; i++ {
		next := checkpointEpisode(time.Duration(i+1) * time.Minute)
		next.IncidentID = inc.ID
		e.ObserveEpisode(context.Background(), next)
		e.mu.Lock()
		e.autoResolve(e.consumeFastFired(map[string]bool{}))
		e.mu.Unlock()
	}
	if n := len(openWithSignal(e, "sre_checkpoint_storm")); n != 1 {
		t.Fatal("an observed episode's incident was auto-resolved")
	}
	for i := 0; i < 2; i++ { // resolution_cycles = 2
		e.mu.Lock()
		e.autoResolve(e.consumeFastFired(map[string]bool{}))
		e.mu.Unlock()
	}
	if n := len(openWithSignal(e, "sre_checkpoint_storm")); n != 0 {
		t.Fatal("the incident stayed open after the episode stopped")
	}
}

func TestObserveEpisode_InvalidEpisodes(t *testing.T) {
	var nilEngine *Engine
	if _, ok := nilEngine.ObserveEpisode(context.Background(), checkpointEpisode(0)); ok {
		t.Fatal("a nil engine recorded an episode")
	}
	e := episodeEngine(t)
	bad := map[string]func(*Episode){
		"no signal":        func(ep *Episode) { ep.Signal = "" },
		"unknown severity": func(ep *Episode) { ep.Severity = "sev1" },
		"no root cause":    func(ep *Episode) { ep.RootCause = "" },
		"no time":          func(ep *Episode) { ep.ObservedAt = time.Time{} },
	}
	for name, mutate := range bad {
		ep := checkpointEpisode(0)
		mutate(&ep)
		if got, ok := e.ObserveEpisode(context.Background(), ep); ok {
			t.Errorf("%s: recorded %+v", name, got)
		}
	}
	if n := len(e.ActiveIncidents()); n != 0 {
		t.Fatalf("invalid episodes left %d incidents", n)
	}
}

// At the open-incident cap the engine refuses new incidents; the
// detector then falls back to its own trigger.
func TestObserveEpisode_RefusedAtTheCap(t *testing.T) {
	e := episodeEngine(t)
	e.mu.Lock()
	for i := 0; i < maxTrackedIncidents; i++ {
		inc := buildIncident(ep0, "warning", []string{fmt.Sprintf("filler_%d", i)},
			"filler", nil, nil, "", "safe")
		inc.DatabaseName = "orders"
		e.insertIncident(&inc)
	}
	e.mu.Unlock()
	if got, ok := e.ObserveEpisode(context.Background(), checkpointEpisode(0)); ok {
		t.Fatalf("an episode was recorded above the cap: %+v", got)
	}
}

// Episodes race with the lock-chain fast path and readers: one incident
// per detector signal, no data race.
func TestObserveEpisode_ConcurrentObservers(t *testing.T) {
	e := episodeEngine(t)
	first, _ := e.ObserveEpisode(context.Background(), checkpointEpisode(0))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				ep := checkpointEpisode(time.Duration(g*20+i) * time.Second)
				ep.IncidentID = first.ID
				e.ObserveEpisode(context.Background(), ep)
				e.ObserveLockChains(context.Background(), nil)
				_ = e.ActiveIncidents()
			}
		}(g)
	}
	wg.Wait()
	got := openWithSignal(e, "sre_checkpoint_storm")
	if len(got) != 1 || got[0].ID != first.ID || got[0].OccurrenceCount != 1 {
		t.Fatalf("open detector incidents = %+v, want only %s", got, first.ID)
	}
}
