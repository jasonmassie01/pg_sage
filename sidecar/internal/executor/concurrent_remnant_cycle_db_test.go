package executor

import (
	"encoding/json"
	"testing"
)

// executor-05 end to end through the executor cycle: an autonomous build
// cancelled by lock_timeout after its catalog entry exists leaves no
// INVALID index, records the dropped OID, and the next cycle builds.
func TestCycleBuildFailureIsCleanedAndRetried(t *testing.T) {
	fx := newLegacyFixture(t, "partial_index", verifiedPartialDetail(), ownRollback)
	fx.exec.cfg.Safety.LockTimeoutMs = 500
	release := holdSnapshot(t, fx.ctx, fx.pool)
	fx.exec.RunCycle(fx.ctx, false)
	release()
	table := "public." + fx.table
	if n := invalidCount(t, fx.ctx, fx.pool, table); n != 0 {
		t.Fatalf("CHECK-01: %d INVALID indexes remain after the failed build", n)
	}
	var raw []byte
	if err := fx.pool.QueryRow(fx.ctx, `SELECT before_state->'remnant_cleanup'
		FROM sage.action_log WHERE finding_id = $1 AND outcome = 'failed'
		ORDER BY id LIMIT 1`, fx.findingID).Scan(&raw); err != nil {
		t.Fatalf("read the failed action: %v", err)
	}
	var c remnantCleanup
	if err := json.Unmarshal(raw, &c); err != nil || c.Outcome != remnantDropped ||
		len(c.Indexes) != 1 || c.Indexes[0].OID <= 0 {
		t.Fatalf("remnant_cleanup = %s (%v), want the dropped OID", raw, err)
	}
	// The lock timeout circuit-breaks the table for the cascade cooldown and
	// the recommendation waits out its retry backoff; both passing is what
	// the next eligible cycle sees. Nothing about the remnant blocks it.
	fx.exec.recentMu.Lock()
	clear(fx.exec.recentActions)
	fx.exec.recentMu.Unlock()
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE sage.recommendation
		SET next_attempt_at = now() WHERE id = $1`, fx.rec.ID); err != nil {
		t.Fatalf("expire the retry backoff: %v", err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	if !fx.indexExists(t, fx.index()) {
		t.Fatalf("CHECK-03: the retry did not build %s; decisions %v", fx.index(),
			fx.decisionVerdicts(t))
	}
	if n := invalidCount(t, fx.ctx, fx.pool, table); n != 0 {
		t.Fatalf("CHECK-03: %d INVALID indexes after the retry", n)
	}
}
