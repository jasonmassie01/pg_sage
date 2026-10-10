package perfgate

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/shadow"
)

// The fixture leaves young pending shadow decisions that the harness
// re-sees during the run, so gate F measures the recurring seen bump on
// sage.shadow_decision, not only the one-time score write. The scorer
// must leave them pending (no operator decision, no applied change, far
// from the horizon), or the bump would find nothing to update.
func TestSeedHistoryLeavesPendingDecisionsToReSee(t *testing.T) {
	pool, ctx, _ := seededPool(t)
	fps := ShadowSeenFingerprints()
	if len(fps) < 2 {
		t.Fatalf("fingerprints to re-see = %v, want several", fps)
	}
	if _, err := shadow.NewScorer(pool, shadow.DefaultOptions(), nil).RunOnce(ctx); err != nil {
		t.Fatalf("score: %v", err)
	}
	store := shadow.NewStore(pool)
	for _, fp := range fps {
		const pending = `SELECT seen_count FROM sage.shadow_decision
			WHERE fingerprint = $1 AND status = 'pending'`
		before := count(t, ctx, pool, pending, fp)
		seen, err := store.Seen(ctx, nil, fp, 24*time.Hour)
		if err != nil || !seen {
			t.Fatalf("Seen(%s) = %t, %v; want a pending decision to bump", fp, seen, err)
		}
		if after := count(t, ctx, pool, pending, fp); after != before+1 {
			t.Fatalf("%s seen_count %d -> %d, want +1", fp, before, after)
		}
		moved := count(t, ctx, pool, `SELECT count(*) FROM sage.shadow_decision
			WHERE fingerprint = $1 AND last_seen_at > recorded_at`, fp)
		if moved != 1 {
			t.Fatalf("%s last_seen_at not bumped past recorded_at", fp)
		}
	}
	if _, err := store.Seen(ctx, nil, "perfgate-no-such-fingerprint", time.Hour); err != nil {
		t.Fatalf("Seen of an unknown fingerprint: %v", err)
	}
}
