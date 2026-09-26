package retention

import (
	"context"
	"testing"
)

const seedIncident = `INSERT INTO sage.incidents
	(severity, root_cause, source, detected_at, last_detected_at, resolved_at)
	VALUES ('info', $1, 'deterministic', now() - $2::interval,
	        now() - $2::interval, now() - $3::interval)`

const seedOpenIncident = `INSERT INTO sage.incidents
	(severity, root_cause, source, detected_at, last_detected_at)
	VALUES ('info', $1, 'deterministic', now() - $2::interval,
	        now() - $2::interval)`

const countIncidents = `SELECT count(*) FROM sage.incidents WHERE root_cause = $1`

// substrate-B7: incidents are pruned by rca.PruneResolvedIncidents, i.e. by
// how long ago they were RESOLVED. The generic rule keyed on last_detected_at
// deleted an incident resolved an hour ago because it was first seen long ago.
func TestRun_PrunesIncidentsByResolutionAge(t *testing.T) {
	_, ctx := requireDB(t)
	recent := uniqueTag("inc_recent_resolve")
	old := uniqueTag("inc_old_resolve")
	open := uniqueTag("inc_open")
	execRetry(t, ctx, seedIncident, recent, "400 days", "1 hour")
	execRetry(t, ctx, seedIncident, old, "400 days", "400 days")
	execRetry(t, ctx, seedOpenIncident, open, "400 days")

	New(testPool, allDays(30), noopLog).Run(ctx)

	if n := countWhere(t, ctx, countIncidents, recent); n != 1 {
		t.Errorf("incident resolved 1h ago: %d rows, want 1 (kept)", n)
	}
	if n := countWhere(t, ctx, countIncidents, old); n != 0 {
		t.Errorf("incident resolved 400d ago: %d rows, want 0 (pruned)", n)
	}
	if n := countWhere(t, ctx, countIncidents, open); n != 1 {
		t.Errorf("open incident: %d rows, want 1 (never pruned)", n)
	}
}

// Boundary: findings_days <= 0 disables incident pruning like every other
// findings-window table.
func TestRun_IncidentPruneDisabledByZeroWindow(t *testing.T) {
	_, ctx := requireDB(t)
	old := uniqueTag("inc_zero_window")
	execRetry(t, ctx, seedIncident, old, "400 days", "400 days")

	cfg := allDays(30)
	cfg.Retention.FindingsDays = 0
	New(testPool, cfg, noopLog).Run(ctx)

	if n := countWhere(t, ctx, countIncidents, old); n != 1 {
		t.Fatalf("findings_days=0 pruned incidents: %d rows, want 1", n)
	}
}

// Error propagation: a prune failure is logged at ERROR with context and
// does not stop the remaining purges.
func TestRun_IncidentPruneFailureIsLogged(t *testing.T) {
	_, ctx := requireDB(t)
	var logged []string
	logFn := func(level, format string, args ...any) {
		if level == "ERROR" {
			logged = append(logged, format)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	c := New(testPool, allDays(30), logFn)
	c.pruneIncidents(cancelled)
	if len(logged) != 1 {
		t.Fatalf("error logs = %v, want exactly one incident prune error", logged)
	}
}
