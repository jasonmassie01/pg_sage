package verify

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// lab-01c on a real server: the watch row (sage.verification's
// baseline JSON) keeps the other verdict and the alpha spend across a
// restart, and two workers resuming the same watch under the real Claim
// store one v2 record, not two. The counters are the fixture's (writing
// snapshots and plans for a 4 h window belongs to lab-01a/b's tests).

func TestV2_PostgresWatchStoresTheOtherVerdictOnce(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	decisionID, actionID := insertVerificationAction(t, ctx, pool)
	t.Cleanup(func() { cleanupVerificationAction(pool, actionID, decisionID) })
	f := newV2Fixture()
	f.request.ID = fmt.Sprintf("v2-db-%d", actionID)
	f.request.ActionID = actionID
	state := WatchStateFromRequest(f.request)
	store := NewPostgresStateStore(pool, 1)
	if err := store.Create(ctx, state); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var claimed atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results, err := f.engine(t, MethodShadow, store).ResumeDueResults(
				context.Background())
			if err != nil {
				t.Errorf("ResumeDueResults: %v", err)
			}
			claimed.Add(int64(len(results)))
		}()
	}
	wg.Wait()
	stored, err := store.Get(ctx, f.request.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claimed.Load() != 1 || stored.AlphaSpend.Looks != 1 || stored.Other == nil ||
		stored.Other.Verifier != VerifierV2 {
		t.Fatalf("claimed %d, looks %d, other %+v; want one claim, one look, a v2 record",
			claimed.Load(), stored.AlphaSpend.Looks, stored.Other)
	}
	rec, _ := stored.Other.Evidence["v2"].(map[string]any)
	if rec["method"] == nil || rec["alpha"] == nil {
		t.Fatalf("stored v2 record %v lacks its method or alpha", rec)
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT baseline->'other'->>'verifier' FROM
		sage.verification WHERE action_log_id = $1`, actionID).Scan(&raw); err != nil ||
		raw != VerifierV2 {
		t.Fatalf("baseline other verifier %q (%v), want v2", raw, err)
	}
}
