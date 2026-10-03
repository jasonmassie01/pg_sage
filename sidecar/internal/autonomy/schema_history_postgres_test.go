package autonomy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// A family row lists every member in target_objects: its dry run counts
// for each member, and the newest row's hash is the identity's last hash.
func TestHistoryCountsFamilyRowsPerMemberAndKeepsTheNewestHash(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	tag := fmt.Sprintf("hist_%d", time.Now().UnixNano())
	members := []string{tag + "_a.events", tag + "_b.events"}
	service := ledger.NewService(ledger.NewPostgresRepository(pool))
	for _, hash := range []string{"old", "new"} {
		if _, err := service.RecordDecision(ctx, ledger.DecisionInput{
			Feature: "schema_guard", Intent: string(schemaguard.InvariantUnboundedAppend),
			Evidence: map[string]any{"disposition": "dry_run",
				"invariant_key": tag, "decision_hash": hash},
			Verdict: ledger.VerdictObserveOnly, Reason: "dry run", RiskTier: "moderate",
			PolicyVersion: 1, TargetObjects: members,
		}); err != nil {
			t.Fatalf("record family row: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.decision
			WHERE evidence->>'invariant_key' = $1`, tag)
	})
	second := schemaguard.Invariant{Kind: schemaguard.InvariantUnboundedAppend,
		Schema: tag + "_b", Table: "events"}
	index, err := (postgresSchemaHistorySource{pool}).History(ctx,
		[]schemaguard.Invariant{second})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if got := index.For(second).SuccessfulRetentionDryRuns; got != 2 {
		t.Fatalf("dry runs for the second member = %d, want 2", got)
	}
	if index.LastHash[tag] != "new" {
		t.Fatalf("last hash = %q, want the newest row's", index.LastHash[tag])
	}
	other := schemaguard.Invariant{Kind: schemaguard.InvariantMissingFKIndex,
		Schema: tag + "_b", Table: "events"}
	if got := index.For(other); got != (schemaguard.History{}) {
		t.Fatalf("another kind on the same table has history %+v", got)
	}
	empty, err := (postgresSchemaHistorySource{pool}).History(ctx, nil)
	if err != nil || len(empty.ByTarget) != 0 || len(empty.LastHash) != 0 {
		t.Fatalf("empty History = %+v, %v", empty, err)
	}
}
