package executor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-07 review: a lease conflict's park is recorded without a
// fingerprint, so no execute resolves it. It carries the candidate key of
// the execute it replaces, so it is listed among the open withheld
// decisions until the candidate's next execute, and not after.
func TestLeaseParkCarriesTheCandidateKeyOffTheWithheldList(t *testing.T) {
	pool, ctx := requireDB(t)
	databaseID := int64(time.Now().UnixNano()%1_000_000_000 + 7_000_000_000)
	key := fmt.Sprintf("ck_policy07_lease_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.decision WHERE database_id = $1`, databaseID)
	})
	insertExecute := func() int64 {
		var id int64
		err := pool.QueryRow(ctx, `INSERT INTO sage.decision (database_id, feature,
			intent, target_objects, policy_version, verdict, risk_tier, reason,
			evidence_id, candidate_key) VALUES ($1, 'index', 'create_index',
			'["public.p07_lease"]', 1, 'execute', 'safe', 'authorized', $2, $3)
			RETURNING id`, databaseID, fmt.Sprintf("p07-lease-%d", time.Now().UnixNano()),
			key).Scan(&id)
		if err != nil {
			t.Fatalf("seed execute decision: %v", err)
		}
		return id
	}
	source := insertExecute()
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	exec.recordLeaseVerdict(ctx, source, leaseVerdictParked, policy.ErrLeaseConflict)

	filter := ledger.WithheldFilter{DatabaseID: &databaseID, Limit: 10}
	open, _, err := ledger.ListOpenWithheld(ctx, pool, filter)
	if err != nil || len(open) != 1 || open[0].CandidateKey != key ||
		open[0].Verdict != ledger.VerdictPark || open[0].Reason != "ddl_conflict" {
		t.Fatalf("open = %+v (%v), want the lease park under the source's key", open, err)
	}
	insertExecute()
	after, _, err := ledger.ListOpenWithheld(ctx, pool, filter)
	if err != nil || len(after) != 0 {
		t.Fatalf("after the next execute = %+v (%v), want no open withheld entry", after, err)
	}
}
