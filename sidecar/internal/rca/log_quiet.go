package rca

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// rca-07: a log event is a point in time, not a state. Clear analyzer
// cycles between two deadlocks say nothing, so a log incident resolves
// only after rca.log_quiet_minutes (default: the dedup window) without an
// occurrence. A linked recurrence soon after its predecessor resolved, at
// no higher severity, is recorded but not paged again
// (rca.recurrence_renotify_hours, default 6). Metric incidents keep
// counting clear cycles and are always paged.

// logIncidentSource is the Source of incidents built from log signals.
const logIncidentSource = "log_deterministic"

// resolveIfQuiet resolves a log incident last seen at least the quiet
// period ago on the engine clock. Caller holds e.mu.
func (e *Engine) resolveIfQuiet(inc *Incident) {
	quiet := e.cfg.LogQuiet()
	if e.clock().Sub(lastSeen(inc)) < quiet {
		return
	}
	e.resolveInMemory(inc, ResolvedByAuto, fmt.Sprintf(
		"no log occurrence for %d minutes (rca.log_quiet_minutes)",
		int(quiet.Minutes())))
}

// suppressRecurrence reports whether the newly inserted log incident inc
// is a recurrence that must not send incident_detected: its predecessor
// resolved less than rca.recurrence_renotify_hours before inc was
// detected, at the same or a higher severity. A severity rise, a metric
// incident, or a predecessor without a resolution always notifies.
func (e *Engine) suppressRecurrence(inc *Incident, r persistResult) bool {
	if inc.Source != logIncidentSource || r.previousID == "" || r.prevResolvedAt == nil {
		return false
	}
	if severityRank(inc.Severity) > severityRank(r.prevSeverity) {
		return false
	}
	return inc.DetectedAt.Sub(*r.prevResolvedAt) < e.cfg.RecurrenceRenotify()
}

// logSuppressed records a suppressed incident_detected at DEBUG.
func (e *Engine) logSuppressed(inc *Incident, r persistResult) {
	e.logFn("debug", "rca: incident %s recurs %s after incident %s resolved "+
		"(severity %s); incident_detected not sent again within "+
		"rca.recurrence_renotify_hours", inc.ID,
		inc.DetectedAt.Sub(*r.prevResolvedAt).Round(time.Second), r.previousID, r.prevSeverity)
}

// joinInts renders ints as a comma-separated list.
func joinInts(vs []int) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}
