package retention

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/facts"
)

// facts-07: the fact history is audit evidence of who lifted a guard. It
// does not age with sage.facts: a fact purged after actions_days keeps its
// events, however old they are.
func TestRun_FactEventsOutliveTheirPurgedFact(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("facts_hist")
	store := facts.NewStore(testPool)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			"DELETE FROM sage.facts WHERE subject LIKE $1 || '%'", tag)
	})
	cite := []facts.Citation{{Kind: "catalog", Ref: tag, Detail: "observed"}}
	f, _, err := store.Propose(ctx, facts.Proposal{Type: facts.TypeSlotConsumer,
		Kind: facts.KindSlot, Subject: tag + "_gone", Source: facts.SourceDetector,
		Value: map[string]string{"consumer": "x"}, Evidence: cite})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Expire(ctx, f.ID, "erin@example.com", "slot dropped"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, `UPDATE sage.facts SET updated_at = now() - interval '400 days'
		WHERE id = $1`, f.ID)
	events := `SELECT count(*) FROM sage.fact_events WHERE fact_id = $1`
	if n := countWhere(t, ctx, events, f.ID); n != 2 {
		t.Fatalf("events before retention = %d, want 2", n)
	}
	// Boundary: one day is the smallest window that purges at all.
	New(testPool, allDays(1), noopLog).Run(ctx)
	if countWhere(t, ctx, `SELECT count(*) FROM sage.facts WHERE id = $1`, f.ID) != 0 {
		t.Fatalf("the expired fact past the actions window was kept")
	}
	if n := countWhere(t, ctx, events+` AND actor = 'erin@example.com'
		AND verb = 'expired'`, f.ID); n != 1 {
		t.Fatalf("the expiry event of a purged fact: %d rows, want 1", n)
	}
	if n := countWhere(t, ctx, events, f.ID); n != 2 {
		t.Fatalf("events after retention = %d, want 2", n)
	}
}

func TestRetentionExemptsFactEvents(t *testing.T) {
	if why := retentionExemptions["fact_events"]; why == "" {
		t.Fatalf("sage.fact_events has no retention exemption")
	}
	for _, r := range purgeRules(allDays(1)) {
		if r.table == "fact_events" {
			t.Fatalf("sage.fact_events must not age out: %+v", r)
		}
	}
}
