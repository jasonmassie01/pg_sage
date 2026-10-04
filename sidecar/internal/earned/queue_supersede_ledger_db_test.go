package earned

import (
	"testing"
)

// Dogfood round 2 item 5: pg_sage now supersedes pending approval items
// whose reason is gone (finding resolved, an index already covers the
// candidate, an equivalent newer proposal). The trust ledger must not read
// supersession or expiry as an operator's "no".
//
// The rule for an operator rejecting an item pg_sage should have
// superseded: the queue decides it. A rejection is recorded only on a
// pending item (store.Reject requires status pending), so once pg_sage has
// detected the defect and superseded the item, no rejection (and no
// demerit) can follow. If the operator acts first, the rejection counts:
// pg_sage proposed something the operator had to turn down, which is
// exactly the evidence the ledger exists to record.

func (f *fixture) closedProposal(status string, decidedBy any) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, status, decided_by, decided_at, action_type, reason)
		VALUES ('CREATE INDEX CONCURRENTLY i ON public.orders (a)', 'moderate', $1, $2,
		        now(), 'create_index_concurrently', 'closed by pg_sage') RETURNING id`,
		status, decidedBy).Scan(&id); err != nil {
		f.t.Fatalf("action_queue: %v", err)
	}
	return id
}

func TestSupersededAndExpiredItemsAreNotRejections(t *testing.T) {
	f := newSelfFixture(t)
	f.grandfatherAt(L3)
	superseded := f.closedProposal("superseded", nil)
	expired := f.closedProposal("expired", nil)
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := f.selfOutcomes()
	if len(got[-superseded]) != 0 || len(got[-expired]) != 0 || res.Demoted != 0 {
		t.Fatalf("superseded/expired items recorded %v / %v, demoted %d; want nothing",
			got[-superseded], got[-expired], res.Demoted)
	}
	if st := f.state(FamilyTuning, ClassIndexCreate); st.Level != L3 {
		t.Fatalf("index_create level = %+v, want L3 kept", st)
	}
}

// The operator rejected the item before pg_sage superseded it: a demerit.
func TestOperatorRejectionOfAStillPendingItemIsADemerit(t *testing.T) {
	f := newSelfFixture(t)
	f.grandfatherAt(L3)
	rejected := f.rejectedProposal("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i ON public.orders (a)", "")
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows := f.selfOutcomes()[-rejected]; len(rows) != 1 || rows[0].result != ResultRejected {
		t.Fatalf("operator rejection recorded %+v, want one rejection", rows)
	}
	if res.Demoted != 1 {
		t.Fatalf("demoted %d, want 1", res.Demoted)
	}
}
