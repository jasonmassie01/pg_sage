package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// minResumeGrace is the least time after a monitor's window during which
// a restarted sidecar still checks the action once (a config restart).
const minResumeGrace = time.Hour

// verificationExpired reports a monitor whose window ended so long ago
// that its before-state no longer describes the workload: the window plus
// a grace of one window (at least minResumeGrace) has passed (dogfood
// lifeos-1: June monitors resumed in October rolled back healthy drops).
func verificationExpired(executedAt time.Time, window time.Duration, now time.Time) bool {
	grace := max(window, minResumeGrace)
	return now.After(executedAt.Add(window + grace))
}

// expireMonitor records an expired monitor: no regression check and no
// rollback, with why. The action is no longer watched; it is not credited.
func expireMonitor(ctx context.Context, pool *pgxpool.Pool, actionID int64,
	executedAt time.Time, window time.Duration, logFn func(string, string, ...any)) {
	ended := executedAt.Add(window)
	reason := fmt.Sprintf("verification window ended %s (%s ago) while pg_sage was not "+
		"watching; comparing that before-state baseline with today's workload would not "+
		"be evidence, so no regression check and no rollback",
		ended.UTC().Format(time.RFC3339), time.Since(ended).Round(time.Minute))
	logFn("rollback", "action %d: %s", actionID, reason)
	if setMonitoredOutcome(ctx, pool, actionID, "verification_expired", reason) {
		_, _ = finalizeActionVerification(ctx, pool, actionID, "unverifiable", reason)
	}
}
