package store

import "testing"

// Owner decision (2026-10-04): a queued item records how it was proposed
// (proposed_via, e.g. "ask_sage") and by whom; items pg_sage queued on
// its own leave both empty.
func TestActionStore_ProposalProvenanceRoundTrips(t *testing.T) {
	pool, ctx := recStorePool(t)
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql)
		VALUES ('provenance', 'info', 'table', 'public.prov_' || md5(random()::text), 't',
		'{}', 'ANALYZE public.prov') RETURNING id`).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	s := NewActionStore(pool)
	asked, err := s.ProposeWithMetadata(ctx, nil, findingID, "ANALYZE public.prov", "", "safe",
		ActionProposalMetadata{ProposedVia: "ask_sage", ProposedBy: "user:42"})
	if err != nil {
		t.Fatal(err)
	}
	own, err := s.Propose(ctx, nil, findingID, "ANALYZE public.prov", "", "safe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.action_queue WHERE id = ANY($1)`,
			[]int{asked, own})
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE id = $1`, findingID)
	})
	a, err := s.GetByID(ctx, asked)
	if err != nil || a.ProposedVia != "ask_sage" || a.ProposedBy != "user:42" {
		t.Fatalf("asked item = %+v (%v)", a, err)
	}
	o, err := s.GetByID(ctx, own)
	if err != nil || o.ProposedVia != "" || o.ProposedBy != "" {
		t.Fatalf("own item = %+v (%v)", o, err)
	}
	if _, err := s.ProposeWithMetadata(ctx, nil, findingID, "ANALYZE public.prov", "", "safe",
		ActionProposalMetadata{ProposedVia: "somewhere_else", ProposedBy: "x"}); err == nil {
		t.Fatal("an unknown proposed_via was stored")
	}
}
