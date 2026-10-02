package executor

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// cascadeCooldown returns the cooldown duration for the cascade
// guard, computed from config.
func (e *Executor) cascadeCooldown() time.Duration {
	cycles := e.cfg.Trust.CascadeCooldownCycles
	interval := e.cfg.Collector.IntervalSeconds
	d := time.Duration(cycles) *
		time.Duration(interval) * time.Second
	if d == 0 {
		d = 5 * time.Minute
	}
	return d
}

// lookupFindingID retrieves the database ID for an open finding.
func (e *Executor) lookupFindingID(
	ctx context.Context, f analyzer.Finding,
) int64 {
	var id int64
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT id FROM sage.findings
		 WHERE category = $1
		   AND object_identifier = $2
		   AND status = 'open'
		   AND acted_on_at IS NULL
		 LIMIT 1`,
		f.Category, f.ObjectIdentifier,
	).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// maxActionRetries is the maximum number of times the executor
// will retry a failed action before giving up permanently.
const maxActionRetries = 3

// oscillationLimit / oscillationWindowDays bound how many times pg_sage
// will repeat the SAME successful action on an object before backing off.
// An object that keeps reverting between cycles — e.g. an app re-creating
// an index pg_sage dropped as redundant — would otherwise churn forever
// (drop → recreate → drop → …). This is a persistent guard (reads the
// action log) so it survives restarts, unlike the in-memory cascade
// cooldown.
const (
	oscillationLimit      = 3
	oscillationWindowDays = 7
)

// exceedsOscillationLimit reports whether pg_sage has already SUCCESSFULLY
// executed this exact action enough times recently that repeating it is
// harmful churn. When it does, the finding is marked acted_on so it stops
// being re-proposed.
func (e *Executor) exceedsOscillationLimit(
	ctx context.Context, f analyzer.Finding, findingID int64,
) bool {
	if e.pool == nil || f.RecommendedSQL == "" {
		return false
	}
	var n int
	if err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT count(*) FROM sage.action_log
		 WHERE sql_executed = $1
		   AND outcome IN ('success', 'rolled_back', 'rollback_failed',
		                   'rollback_skipped')
		   AND executed_at > now() - make_interval(days => $2)`,
		f.RecommendedSQL, oscillationWindowDays,
	).Scan(&n); err != nil {
		return false
	}
	if n >= oscillationLimit {
		e.logFn("executor",
			"anti-oscillation: already applied %q %d times in %dd — "+
				"backing off (object reverts externally)",
			f.Title, n, oscillationWindowDays)
		_, _ = e.pool.Exec(ctx,
			`/* pg_sage */ UPDATE sage.findings SET acted_on_at = now()
			 WHERE id = $1 AND acted_on_at IS NULL`, findingID)
		return true
	}
	return false
}

// exceedsMaxRetries checks if a finding has already failed more
// than maxActionRetries times, preventing infinite retry loops.
func (e *Executor) exceedsMaxRetries(
	ctx context.Context, findingID int64,
) bool {
	if e.pool == nil {
		return false
	}
	var failCount int
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT count(*) FROM sage.action_log al
		 LEFT JOIN sage.findings prev ON prev.id = al.finding_id
		 WHERE al.outcome = 'failed'
		   AND (al.finding_id = $1 OR EXISTS (
		        SELECT 1 FROM sage.findings cur WHERE cur.id = $1
		           AND cur.category = prev.category
		           AND cur.object_identifier = prev.object_identifier))`,
		findingID,
	).Scan(&failCount)
	if err != nil {
		return false // on error, allow retry
	}
	if failCount >= maxActionRetries {
		// Mark the finding as acted_on so it stops appearing.
		// If the UPDATE fails silently we would retry forever, so
		// log the failure to surface it to operators.
		if _, err := e.pool.Exec(ctx,
			`/* pg_sage */ UPDATE sage.findings
			 SET acted_on_at = now()
			 WHERE id = $1 AND acted_on_at IS NULL`,
			findingID,
		); err != nil {
			e.logFn("executor",
				"failed to mark finding %d acted_on after "+
					"%d retries: %v — will retry this update next cycle",
				findingID, failCount, err)
		}
		return true
	}
	return false
}

func (e *Executor) markFindingActioned(
	ctx context.Context,
	findingID int64,
	actionID int64,
) {
	tag, err := e.pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.findings
		 SET acted_on_at = now(),
		     action_log_id = $1,
		     status = 'resolved',
		     resolved_at = COALESCE(resolved_at, now())
		 WHERE id = $2`,
		actionID, findingID,
	)
	if err != nil {
		e.logFn("executor",
			"failed to mark finding %d resolved after action %d: %v",
			findingID, actionID, err)
		return
	}
	if tag.RowsAffected() == 0 {
		e.logFn("executor",
			"manual action %d did not resolve finding %d: finding missing or already changed",
			actionID, findingID)
	}
}
