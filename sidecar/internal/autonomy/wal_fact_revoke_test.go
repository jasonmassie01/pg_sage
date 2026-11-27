package autonomy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/custodian/wal"
	"github.com/pg-sage/sidecar/internal/facts"
)

// facts-04: the WAL custodian's evidence follows a slot consumer fact
// through confirm, reject and confirm again, so a revoked fact no longer
// makes the custodian treat its slot as a registered consumer.
func TestWALCustodianEvidenceFollowsASlotFactRevocation(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	slot := fmt.Sprintf("facts04_cdc_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.facts WHERE subject = $1", slot)
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.slot_consumer_registry WHERE slot_name = $1", slot)
	})
	store := facts.NewStore(pool)
	f, _, err := store.Propose(ctx, facts.Proposal{Type: facts.TypeSlotConsumer,
		Kind: facts.KindSlot, Subject: slot, Source: facts.SourceDetector,
		Value:    map[string]string{"consumer": "debezium"},
		Evidence: []facts.Citation{{Kind: "catalog", Ref: "slot:" + slot, Detail: "seen"}}})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	custodian := NewPostgresWALCustodian(pool, "testdb", PostgresWALOptions{})
	evidence := func() wal.SlotEvidence {
		return custodian.slotEvidence(ctx, slot, wal.SlotTypeLogical, false, 1<<30)
	}
	steps := []struct {
		confirm    bool
		registered bool
	}{{true, true}, {false, false}, {true, true}, {false, false}}
	for i, step := range steps {
		if _, err := store.Decide(ctx, f.ID, facts.Decision{Confirm: step.confirm,
			Actor: "op@example.com"}); err != nil {
			t.Fatalf("step %d decide(confirm=%v): %v", i, step.confirm, err)
		}
		got := evidence()
		if !got.RegistryEvidenceKnown || got.ConsumerRegistered != step.registered ||
			got.OwnerTag != "debezium" {
			t.Fatalf("step %d (confirm=%v): evidence %+v, want registered=%v", i,
				step.confirm, got, step.registered)
		}
	}
}
