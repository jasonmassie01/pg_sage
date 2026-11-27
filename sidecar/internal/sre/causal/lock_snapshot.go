package causal

import (
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Detection-time roots (sre-05). An investigation probes at least one
// trigger poll after detection, when many lock chains have cleared. The
// incident's recorded root blocker then says who blocked whom at
// detection: a contributing hypothesis for the operator, never the root
// (the session may have committed, and its pid may be reused). A usable
// non-empty live graph always wins and ignores the snapshot.

// detectedBlockers keeps the detection-time snapshot's blockers.
func detectedBlockers(blockers []BlockerEvidence) []BlockerEvidence {
	var out []BlockerEvidence
	for _, b := range blockers {
		if !b.DetectedAt.IsZero() {
			out = append(out, b)
		}
	}
	return out
}

// when prefixes the facts of a detection-time root.
func (r lockRoot) when() string {
	if r.detectedAt.IsZero() {
		return ""
	}
	return "at detection, "
}

// detectionOnly turns a diagnosis rooted on a detection-time blocker into
// an inconclusive one: its root becomes a contributing hypothesis and
// the reason names the blocker as it was at detection.
func detectionOnly(d Diagnosis, r lockRoot, obs []Observation) Diagnosis {
	if d.Root != nil {
		h := *d.Root
		h.Status = StatusContributing
		d.Contributing = append([]Hypothesis{h}, d.Contributing...)
	}
	d.Root, d.Conclusive = nil, false
	d.Reason = detectionReason(r, obs)
	return d
}

// detectionReason says why there is no live root and what the snapshot
// recorded, with its age against the lock graph probe when both are dated.
func detectionReason(r lockRoot, obs []Observation) string {
	lead := "lock graph unavailable"
	var probedAt time.Time
	if g, ok := find(obs, probes.LockGraph); ok {
		if g.Result.Status.Usable() {
			lead = "chain cleared before probing"
		}
		probedAt = g.Result.ObservedAt
	}
	state := r.state
	if state == "" {
		state = "state unknown"
	}
	start := "backend_start unknown"
	if !r.backendStart.IsZero() {
		start = "backend_start " + r.backendStart.UTC().Format(time.RFC3339)
	}
	out := fmt.Sprintf("%s; at detection pid %d (%s, %s) blocked %d sessions", lead, r.pid,
		state, start, r.waiters)
	if !probedAt.IsZero() && probedAt.After(r.detectedAt) {
		age := math.Round(probedAt.Sub(r.detectedAt).Seconds())
		out += fmt.Sprintf("; the detection snapshot is %s s older than the probes",
			probes.FormatValue(age))
	}
	return out
}
