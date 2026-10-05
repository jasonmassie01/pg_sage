package perfgate

import (
	"slices"
	"testing"
)

// sage.action_outcome grows with every verified action (one row per
// executed action). The gate seeded none, so the reads that scan it were
// invisible to gate A; it is now seeded like the other growing tables,
// as history: decided long enough ago that the runtime has nothing to
// ingest from it (the trust reconciler looks back 30 days).
func TestSeedHistorySeedsActionOutcomesAsHistory(t *testing.T) {
	if !slices.Contains(GrowingTables(), "action_outcome") {
		t.Fatalf("growing tables %v lack action_outcome", GrowingTables())
	}
	pool, ctx, _ := seededPool(t)
	s := tinyScale()
	n := count(t, ctx, pool, `SELECT count(*) FROM sage.action_outcome`)
	if n < int64(s.HistoryRows) {
		t.Fatalf("action_outcome has %d rows, want >= %d", n, s.HistoryRows)
	}
	orphans := count(t, ctx, pool, `SELECT count(*) FROM sage.action_outcome o
		WHERE NOT EXISTS (SELECT 1 FROM sage.action_log l WHERE l.id = o.action_log_id)`)
	if orphans != 0 {
		t.Fatalf("%d outcomes without an action", orphans)
	}
	recent := count(t, ctx, pool, `SELECT count(*) FROM sage.action_outcome
		WHERE decided_at > now() - interval '31 days' OR decided_at IS NULL`)
	if recent != 0 {
		t.Fatalf("%d outcomes are live work (pending or decided in the last 31 days)", recent)
	}
	verdicts := count(t, ctx, pool, `SELECT count(DISTINCT verdict) FROM sage.action_outcome`)
	if verdicts < 4 {
		t.Fatalf("outcomes span %d verdicts, want several", verdicts)
	}
}
