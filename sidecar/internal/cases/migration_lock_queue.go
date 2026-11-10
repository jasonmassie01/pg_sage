package cases

import (
	"time"
)

const lockQueueIdentityReason = "lock-queue DDL has no complete backend identity"

// lockQueueCancelCandidate is the approval-only cancel of an in-flight DDL
// that holds a lock queue (migration-05): a guarded cancel_backend script
// for the recorded DDL backend, at HIGH risk (HIGH always needs approval)
// and offered as a script only, never queued. An incomplete identity
// yields a blocked candidate with no SQL.
func lockQueueCancelCandidate(f SourceFinding, expires time.Time) (ActionCandidate, bool) {
	if !detailBool(f.Detail, "lock_queue_escalated", false) {
		return ActionCandidate{}, false
	}
	confidence := migrationSafetyConfidence(f)
	b, ok := lockQueueDDLBackend(f.Detail)
	var c ActionCandidate
	if ok {
		c = guardedSignalCandidate("cancel_backend", b, expires, confidence)
	} else {
		c = blockedSignalCandidate("cancel_backend", lockQueueIdentityReason, expires,
			confidence)
	}
	c.RiskTier = "high"
	c.VerificationPlan = []string{"refresh lock graph; verify the queue behind the " +
		"DDL drained", "retry the DDL with SET lock_timeout = '5s'"}
	return c, true
}

// lockQueueDDLBackend reads the DDL backend identity the migration
// detector records (pid, backend_start, query_start, query_sha, state).
func lockQueueDDLBackend(detail map[string]any) (IncidentBlocker, bool) {
	b := IncidentBlocker{
		PID:      int(detailInt64(detail, "pid", 0)),
		QuerySHA: detailString(detail, "query_sha", ""),
		State:    detailString(detail, "state", ""),
	}
	b.BackendStart = detailTime(detail, "backend_start")
	b.QueryStart = detailTime(detail, "query_start")
	if b.PID <= 0 || b.BackendStart.IsZero() || b.QueryStart.IsZero() ||
		b.State != stateActive || !querySHAPattern.MatchString(b.QuerySHA) {
		return IncidentBlocker{}, false
	}
	return b, true
}

// detailTime reads a timestamp from a detail map, as a time.Time or as the
// RFC 3339 text JSON stores; anything else is zero.
func detailTime(detail map[string]any, key string) time.Time {
	switch v := detail[key].(type) {
	case time.Time:
		return v
	case string:
		t, err := time.Parse(time.RFC3339Nano, v)
		if err == nil {
			return t
		}
	}
	return time.Time{}
}
