package retention

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/facts"
)

// facts-04: a retention run past the actions window keeps rejected facts,
// so ImportDeclared, which reads declarations that still stand (an
// append_only contract, a still-registered slot), finds the rejection and
// imports nothing; an expired fact still ages out.
func TestRun_RejectedFactsSurviveSoDeclarationsCannotResurrectThem(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("facts_tomb")
	store := facts.NewStore(testPool)
	t.Cleanup(func() { cleanTombstoneRows(tag) })
	appendOnly, slotFact, expired := seedTombstoneFacts(t, ctx, store, tag)
	mustExec(t, ctx, `UPDATE sage.facts SET updated_at = now() - interval '400 days'
		WHERE subject LIKE '%' || $1 || '%'`, tag)
	rejectedBefore := countWhere(t, ctx, `SELECT count(*) FROM sage.facts
		WHERE status = 'rejected'`)
	// Boundary: one day is the smallest window that purges at all (0 disables).
	New(testPool, allDays(1), noopLog).Run(ctx)
	if got := countWhere(t, ctx, `SELECT count(*) FROM sage.facts
		WHERE status = 'rejected'`); got != rejectedBefore {
		t.Fatalf("rejected facts %d after retention, want %d", got, rejectedBefore)
	}
	if countWhere(t, ctx, `SELECT count(*) FROM sage.facts WHERE id = $1`,
		expired) != 0 {
		t.Fatalf("an expired fact past the window was kept")
	}
	if _, err := store.ImportDeclared(ctx); err != nil {
		t.Fatalf("import declared: %v", err)
	}
	for _, id := range []int64{appendOnly, slotFact} {
		f, err := store.Get(ctx, id)
		if err != nil || f.Status != facts.StatusRejected {
			t.Fatalf("fact %d after retention and import: %+v %v", id, f, err)
		}
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.facts
		WHERE subject LIKE '%' || $1 || '%' AND status = 'confirmed'`, tag); n != 0 {
		t.Fatalf("%d declarations were re-imported as confirmed facts", n)
	}
}

// seedTombstoneFacts declares an append_only table and a registered slot,
// rejects a fact about each (the slot is registered again afterwards, as an
// earlier release left it or register_consumer would), and expires a third
// fact. It returns the three fact ids.
func seedTombstoneFacts(t *testing.T, ctx context.Context, store *facts.Store,
	tag string) (int64, int64, int64) {
	t.Helper()
	table, slot := tag+"_ledger", tag+"_cdc"
	mustExec(t, ctx, `INSERT INTO sage.table_contract (schema_name, table_name,
		append_only, declared_by, evidence_id) VALUES ('app', $1, true, 'mcp:dev', $1)`, table)
	mustExec(t, ctx, `INSERT INTO sage.slot_consumer_registry (slot_name, owner_tag,
		registered) VALUES ($1, 'debezium', true)`, slot)
	cite := []facts.Citation{{Kind: "catalog", Ref: tag, Detail: "observed"}}
	appendOnly := rejectedFact(t, ctx, store, facts.Proposal{Type: facts.TypeAppendOnly,
		Kind: facts.KindTable, Subject: "app." + table, Source: facts.SourceModel,
		Evidence: cite})
	slotFact := rejectedFact(t, ctx, store, facts.Proposal{Type: facts.TypeSlotConsumer,
		Kind: facts.KindSlot, Subject: slot, Source: facts.SourceDetector,
		Value: map[string]string{"consumer": "debezium"}, Evidence: cite})
	mustExec(t, ctx, `UPDATE sage.slot_consumer_registry SET registered = true
		WHERE slot_name = $1`, slot)
	expired, _, err := store.Propose(ctx, facts.Proposal{Type: facts.TypeSlotConsumer,
		Kind: facts.KindSlot, Subject: tag + "_gone", Source: facts.SourceDetector,
		Value: map[string]string{"consumer": "x"}, Evidence: cite})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Expire(ctx, expired.ID, "ops", "slot dropped"); err != nil {
		t.Fatal(err)
	}
	return appendOnly, slotFact, expired.ID
}

func rejectedFact(t *testing.T, ctx context.Context, s *facts.Store, p facts.Proposal) int64 {
	t.Helper()
	f, _, err := s.Propose(ctx, p)
	if err != nil {
		t.Fatalf("propose %s: %v", p.Subject, err)
	}
	r, err := s.Decide(ctx, f.ID, facts.Decision{Actor: "carol@example.com"})
	if err != nil || r.Status != facts.StatusRejected {
		t.Fatalf("reject %s: %+v %v", p.Subject, r, err)
	}
	return f.ID
}

func mustExec(t *testing.T, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func cleanTombstoneRows(tag string) {
	ctx := context.Background()
	_, _ = testPool.Exec(ctx, "DELETE FROM sage.facts WHERE subject LIKE '%' || $1 || '%'", tag)
	_, _ = testPool.Exec(ctx, "DELETE FROM sage.table_contract WHERE table_name LIKE $1 || '%'",
		tag)
	_, _ = testPool.Exec(ctx,
		"DELETE FROM sage.slot_consumer_registry WHERE slot_name LIKE $1 || '%'", tag)
}
