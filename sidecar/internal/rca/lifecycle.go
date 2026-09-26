package rca

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Resolution actors recorded in sage.incidents.resolved_by by the engine.
// Operators resolving through the API are recorded as "user:<email>".
const (
	ResolvedByAuto      = "pg_sage:auto_resolve"
	ResolvedBySupersede = "pg_sage:superseded"
	supersededReason    = "superseded: condition re-detected after a gap " +
		"longer than the dedup window"
	maxTrackedIncidents  = 500
	defaultDedupWindow   = 30 * time.Minute
	defaultResolveCycles = 2
	defaultEscalateCycle = 5
)

// trackState is the persistence and notification bookkeeping for one
// in-memory incident.
type trackState struct {
	persisted       bool // row exists in sage.incidents
	notifyDetected  bool // incident_detected pending
	notifyEscalated bool // incident_escalated pending
}

// identityString is the stable identity of an incident: two detections
// with the same identity are the same incident while it is open.
func identityString(inc *Incident) string {
	first := ""
	if len(inc.AffectedObjects) > 0 {
		first = inc.AffectedObjects[0]
	}
	return strings.Join([]string{
		inc.Source, inc.DatabaseName,
		strings.Join(sortedCopy(inc.SignalIDs), ","), first,
	}, "\x1f")
}

// identityKey is the hashed identity persisted in sage.incidents so a
// recurrence can be linked to the incident it follows across restarts.
func identityKey(inc *Incident) string {
	sum := sha256.Sum256([]byte(identityString(inc)))
	return hex.EncodeToString(sum[:])
}

func (e *Engine) trackFor(id string) *trackState {
	ts, ok := e.track[id]
	if !ok {
		ts = &trackState{}
		e.track[id] = ts
	}
	return ts
}

func (e *Engine) dedupWindow() time.Duration {
	if e.cfg.DedupWindowMinutes > 0 {
		return time.Duration(e.cfg.DedupWindowMinutes) * time.Minute
	}
	return defaultDedupWindow
}

// dedup merges inc into a matching open incident, or records it as new.
// An open match last seen longer ago than the dedup window is resolved as
// superseded and the new incident is linked to it.
func (e *Engine) dedup(inc *Incident) {
	key := identityString(inc)
	for i := range e.incidents {
		existing := &e.incidents[i]
		if existing.ResolvedAt != nil || identityString(existing) != key {
			continue
		}
		lastSeen := existing.LastDetectedAt
		if lastSeen.IsZero() {
			lastSeen = existing.DetectedAt
		}
		if inc.DetectedAt.Sub(lastSeen) > e.dedupWindow() {
			e.resolveInMemory(existing, ResolvedBySupersede, supersededReason)
			inc.PreviousIncidentID = existing.ID
			break
		}
		existing.OccurrenceCount++
		existing.LastDetectedAt = inc.DetectedAt
		if severityRank(inc.Severity) > severityRank(existing.Severity) {
			existing.Severity = inc.Severity
		}
		delete(e.clearCounts, existing.ID)
		return
	}
	e.insertIncident(inc)
}

func (e *Engine) insertIncident(inc *Incident) {
	if e.activeCount() >= maxTrackedIncidents {
		if !e.capWarned {
			e.logFn("warn", "rca: %d open incidents tracked; not "+
				"recording new incident %q until some resolve",
				maxTrackedIncidents, inc.RootCause)
			e.capWarned = true
		}
		return
	}
	e.capWarned = false
	inc.ID = newUUID()
	inc.OccurrenceCount = 1
	inc.LastDetectedAt = inc.DetectedAt
	e.incidents = append(e.incidents, *inc)
	e.trackFor(inc.ID).notifyDetected = true
}

func (e *Engine) activeCount() int {
	n := 0
	for i := range e.incidents {
		if e.incidents[i].ResolvedAt == nil {
			n++
		}
	}
	return n
}

func (e *Engine) resolveInMemory(inc *Incident, by, reason string) {
	now := time.Now()
	inc.ResolvedAt = &now
	inc.ResolvedBy = by
	inc.ResolutionReason = reason
	delete(e.clearCounts, inc.ID)
	e.logFn("info", "rca: resolved incident %s (%s) by %s",
		inc.ID, inc.RootCause, by)
}

func (e *Engine) autoResolve(firedIDs map[string]bool) {
	if e.gracePeriodLeft > 0 {
		return
	}
	needed := e.cfg.ResolutionCycles
	if needed == 0 {
		needed = defaultResolveCycles
	}
	for i := range e.incidents {
		inc := &e.incidents[i]
		if inc.ResolvedAt != nil {
			continue
		}
		if anyFired(inc.SignalIDs, firedIDs) {
			delete(e.clearCounts, inc.ID)
			continue
		}
		e.clearCounts[inc.ID]++
		if e.clearCounts[inc.ID] >= needed {
			e.resolveInMemory(inc, ResolvedByAuto, fmt.Sprintf(
				"signals cleared for %d consecutive cycles", needed))
		}
	}
}

func anyFired(ids []string, fired map[string]bool) bool {
	for _, sid := range ids {
		if fired[sid] {
			return true
		}
	}
	return false
}

func (e *Engine) escalate() {
	needed := e.cfg.EscalationCycles
	if needed == 0 {
		needed = defaultEscalateCycle
	}
	for i := range e.incidents {
		inc := &e.incidents[i]
		if inc.ResolvedAt != nil || inc.EscalatedAt != nil {
			continue
		}
		if inc.Severity != "warning" || inc.OccurrenceCount < needed {
			continue
		}
		now := time.Now()
		inc.Severity = "critical"
		inc.EscalatedAt = &now
		e.trackFor(inc.ID).notifyEscalated = true
		e.logFn("warn", "rca: escalated incident %s to critical (%s)",
			inc.ID, inc.RootCause)
	}
}

// trimResolvedOverflow bounds memory when resolutions cannot be made
// durable (no store bound, or the database is unreachable): the oldest
// resolved incidents are dropped once the slice exceeds twice the cap.
func (e *Engine) trimResolvedOverflow() {
	excess := len(e.incidents) - 2*maxTrackedIncidents
	if excess <= 0 {
		return
	}
	kept := e.incidents[:0]
	dropped := 0
	for _, inc := range e.incidents {
		if dropped < excess && inc.ResolvedAt != nil {
			delete(e.track, inc.ID)
			dropped++
			continue
		}
		kept = append(kept, inc)
	}
	e.incidents = kept
	e.logFn("warn", "rca: dropped %d unpersisted resolved incidents "+
		"from memory", dropped)
}

// removeIncidents drops the given incident IDs from memory.
func (e *Engine) removeIncidents(ids map[string]bool) {
	if len(ids) == 0 {
		return
	}
	kept := e.incidents[:0]
	for _, inc := range e.incidents {
		if ids[inc.ID] {
			delete(e.track, inc.ID)
			delete(e.clearCounts, inc.ID)
			continue
		}
		kept = append(kept, inc)
	}
	e.incidents = kept
}
