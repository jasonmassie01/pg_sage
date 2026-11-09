package auditchain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// facts-07: the fact history (sage.fact_events) is a built-in, append-only
// chain, so verification and the SIEM export cover who lifted a guard.

// seedFactEvents writes a fact and decides it, which the facts trigger
// records as two events; it returns the event ids.
func seedFactEvents(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	exec(t, pool, `INSERT INTO sage.facts (fact_type, subject_kind, subject, source,
		proposed_by) VALUES ('test_fixture', 'schema', 'fx_*', 'detector', 'detector:x')`)
	exec(t, pool, `UPDATE sage.facts SET status = 'confirmed', decided_by = 'alice',
		decided_at = now() WHERE subject = 'fx_*'`)
	rows, err := pool.Query(context.Background(),
		`SELECT id FROM sage.fact_events ORDER BY id`)
	if err != nil {
		t.Fatalf("read fact events: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 2 {
		t.Fatalf("fact events = %v, want 2", ids)
	}
	return ids
}

func TestFactEventsChainIsBuiltinAndAppendOnly(t *testing.T) {
	spec, ok := auditchain.SpecFor("fact_events")
	if !ok || spec.Chain != auditchain.FactEvents.Chain {
		t.Fatalf("fact_events is not a built-in chain: %+v %v", spec, ok)
	}
	if spec.Table != "sage.fact_events" || len(spec.State) != 0 || spec.Optional {
		t.Fatalf("fact_events spec = %+v, want append-only and always installed", spec)
	}
}

// Integration: an intact history verifies; editing an event behind the
// triggers fails verification on that row.
func TestFactEventsChainDetectsAnEditedEvent(t *testing.T) {
	pool := freshDB(t, "chain_fact_events")
	ids := seedFactEvents(t, pool)
	rep := verify(t, pool, auditchain.FactEvents, auditchain.Window{})
	if !rep.OK() || rep.Links != 2 || rep.RowsChecked != 2 {
		t.Fatalf("intact fact history report = %+v", rep)
	}
	bypass(t, pool, `UPDATE sage.fact_events SET actor = 'mallory' WHERE id = $1`, ids[1])
	rep = verify(t, pool, auditchain.FactEvents, auditchain.Window{})
	kinds := problemKinds(rep)[ids[1]]
	if rep.OK() || len(kinds) == 0 || kinds[0] != auditchain.KindRowEdited {
		t.Fatalf("edited event report = %+v, want row_edited on %d", rep, ids[1])
	}
}

// An ordinary update is refused outright: the history is append-only,
// and only the chain's own columns may be written after the insert.
func TestFactEventsRefuseAnUpdate(t *testing.T) {
	pool := freshDB(t, "chain_fact_events_upd")
	ids := seedFactEvents(t, pool)
	_, err := pool.Exec(context.Background(),
		`UPDATE sage.fact_events SET note = 'rewritten' WHERE id = $1`, ids[0])
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update of a fact event: %v, want an append-only refusal", err)
	}
	rep := verify(t, pool, auditchain.FactEvents, auditchain.Window{})
	if !rep.OK() || rep.Links != 2 {
		t.Fatalf("history after the refused update = %+v", rep)
	}
}
