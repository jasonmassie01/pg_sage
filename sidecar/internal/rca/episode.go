package rca

import (
	"context"
	"time"
)

// Sage SRE M6 reactive detector episodes. The detector samples checkpoint
// and temp-file counters and LWLock waits on every trigger poll and
// decides, deterministically, when an episode opens. The engine turns
// each episode into an incident like any other, so incident identity,
// dedup, escalation, auto-resolution, persistence and notifications stay
// in one place: the first observation opens an incident (or attaches to
// an open one), later observations of the same episode refresh it and keep
// it from auto-resolving, and an incident resolved during its episode is
// never reopened by that episode.

// Episode is one observation of a detector episode.
type Episode struct {
	// Signal is the detector's incident signal id.
	Signal string
	// Related are RCA signals of the same family: an open incident with
	// one of them and at least this severity is the episode's incident.
	Related   []string
	Severity  string
	RootCause string
	// Evidence is the measurement against the detector's threshold.
	Evidence   string
	ObservedAt time.Time
	// IncidentID is the incident the episode was recorded as; empty on
	// the episode's first observation.
	IncidentID string
}

// detectorDescription labels the detector's evidence in the causal chain.
const detectorDescription = "Sage SRE reactive detector episode"

// ObserveEpisode records one observation of a detector episode and
// returns the open incident it belongs to. ok is false for an invalid
// episode, at the open-incident cap, and when the episode's incident is
// no longer open (resolved by an operator or superseded): that episode
// never opens a second incident. The caller persists (PersistIncidents),
// which also notifies.
func (e *Engine) ObserveEpisode(ctx context.Context, ep Episode) (Incident, bool) {
	if e == nil || !validEpisode(ep) {
		return Incident{}, false
	}
	e.syncResolved(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if ep.IncidentID != "" {
		return e.refreshEpisode(ep)
	}
	return e.openEpisode(ep)
}

func validEpisode(ep Episode) bool {
	return ep.Signal != "" && ep.RootCause != "" && !ep.ObservedAt.IsZero() &&
		severityRank(ep.Severity) > 0
}

// episodeIncident is the incident a detector episode opens.
func episodeIncident(ep Episode, database string) Incident {
	inc := buildIncident(ep.ObservedAt, ep.Severity, []string{ep.Signal}, ep.RootCause,
		[]ChainLink{{Order: 1, Signal: ep.Signal, Description: detectorDescription,
			Evidence: ep.Evidence}}, nil, "", "")
	inc.DatabaseName = database
	return inc
}

// refreshEpisode applies a later observation to the episode's incident.
// An attached RCA incident is linked, never rewritten. Caller holds e.mu.
func (e *Engine) refreshEpisode(ep Episode) (Incident, bool) {
	for i := range e.incidents {
		inc := &e.incidents[i]
		if inc.ID != ep.IncidentID {
			continue
		}
		if inc.ResolvedAt != nil {
			return Incident{}, false
		}
		if containsSignal(inc.SignalIDs, ep.Signal) {
			fresh := episodeIncident(ep, e.databaseName)
			refreshOpen(inc, &fresh)
			delete(e.clearCounts, inc.ID)
			e.markFired(ep.Signal)
		}
		return *inc, true
	}
	return Incident{}, false
}

// openEpisode records an episode's first observation: its own open
// incident counts one more occurrence, else an open related incident of
// at least its severity is linked, else a new incident opens. Caller
// holds e.mu.
func (e *Engine) openEpisode(ep Episode) (Incident, bool) {
	fresh := episodeIncident(ep, e.databaseName)
	key := identityString(&fresh)
	if _, own := e.openByIdentity(key); !own {
		if rel, ok := e.relatedOpen(ep); ok {
			return rel, true
		}
	}
	e.merge(&fresh, true)
	e.markFired(ep.Signal)
	return e.openByIdentity(key)
}

func (e *Engine) openByIdentity(key string) (Incident, bool) {
	for i := range e.incidents {
		inc := &e.incidents[i]
		if inc.ResolvedAt == nil && identityString(inc) == key {
			return *inc, true
		}
	}
	return Incident{}, false
}

// relatedOpen is the most recently seen open incident of this database
// carrying one of the episode's related signals at its severity or above.
func (e *Engine) relatedOpen(ep Episode) (Incident, bool) {
	var best *Incident
	for i := range e.incidents {
		inc := &e.incidents[i]
		if inc.ResolvedAt != nil || inc.DatabaseName != e.databaseName ||
			severityRank(inc.Severity) < severityRank(ep.Severity) {
			continue
		}
		related := false
		for _, sig := range ep.Related {
			related = related || containsSignal(inc.SignalIDs, sig)
		}
		if related && (best == nil || inc.LastDetectedAt.After(best.LastDetectedAt)) {
			best = inc
		}
	}
	if best == nil {
		return Incident{}, false
	}
	return *best, true
}

// markFired keeps an observed signal firing until the next analyzer
// cycle, which defers auto-resolution like the fast path. Caller holds
// e.mu.
func (e *Engine) markFired(signal string) {
	if e.fastFired == nil {
		e.fastFired = make(map[string]bool)
	}
	e.fastFired[signal] = true
}

func containsSignal(ids []string, signal string) bool {
	for _, id := range ids {
		if id == signal {
			return true
		}
	}
	return false
}
