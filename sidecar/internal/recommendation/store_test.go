package recommendation

import (
	"errors"
	"testing"
)

const (
	sqlA     = "CREATE INDEX CONCURRENTLY idx_a ON public.orders (a)"
	inverseA = "DROP INDEX CONCURRENTLY public.idx_a"
	// sqlA2 is the same index definition under a new name: the same
	// identity (C05 fingerprints ignore the name) with new content (C04).
	sqlA2    = "CREATE INDEX CONCURRENTLY idx_b ON public.orders (a)"
	inverseB = "DROP INDEX CONCURRENTLY public.idx_b"
)

func TestProposeCreatesHeadRevisionAndHistory(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	p := proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)
	v := int64(4)
	p.PolicyVersion = &v
	res := mustPropose(t, ctx, s, p)
	rec := res.Recommendation
	if res.Outcome != OutcomeCreated || rec.State != StateProposed || rec.Revision != 1 {
		t.Fatalf("propose = %s state=%s rev=%d, want created proposed 1",
			res.Outcome, rec.State, rec.Revision)
	}
	if rec.IdentityKey != IdentityKey(p) || rec.ContentHash != ContentHash(p) {
		t.Fatalf("identity/content = %s/%s, want %s/%s",
			rec.IdentityKey, rec.ContentHash, IdentityKey(p), ContentHash(p))
	}
	if rec.FindingID == nil || *rec.FindingID <= 0 {
		t.Fatalf("finding_id = %v, want the open finding", rec.FindingID)
	}
	if rec.ActionType != "create_index" || rec.IndexFingerprint == "" ||
		rec.RetryBudget != DefaultRetryBudget || rec.ApprovedHash != "" {
		t.Fatalf("head = %+v", rec)
	}
	revs, err := s.Revisions(ctx, rec.ID)
	if err != nil || len(revs) != 1 {
		t.Fatalf("revisions = %d, %v, want 1", len(revs), err)
	}
	r := revs[0]
	if r.ForwardSQL != sqlA || r.InverseSQL != inverseA || r.ContentHash != rec.ContentHash ||
		r.Source != SourceAnalyzer || r.PolicyVersion == nil || *r.PolicyVersion != 4 ||
		r.Evidence["seq_scans"] != float64(10) || r.Preconditions["target"] != p.Target {
		t.Fatalf("revision = %+v", r)
	}
	h := transitionsOf(t, ctx, s, rec.ID)
	if len(h) != 1 || h[0].From != "" || h[0].To != StateProposed || h[0].Revision != 1 {
		t.Fatalf("history = %+v, want one creation record", h)
	}
}

func TestProposeUnchangedContentWritesNoRevision(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	p := proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)
	first := mustPropose(t, ctx, s, p).Recommendation
	p.Evidence = map[string]any{"seq_scans": 5000}
	p.Title = "reworded"
	second := mustPropose(t, ctx, s, p)
	if second.Outcome != OutcomeUnchanged || second.Recommendation.ID != first.ID ||
		second.Recommendation.Revision != 1 {
		t.Fatalf("second propose = %s id=%d rev=%d", second.Outcome,
			second.Recommendation.ID, second.Recommendation.Revision)
	}
	if revs, _ := s.Revisions(ctx, first.ID); len(revs) != 1 {
		t.Fatalf("unchanged content wrote %d revisions, want 1", len(revs))
	}
	if !second.Recommendation.LastSeenAt.After(first.LastSeenAt) &&
		!second.Recommendation.LastSeenAt.Equal(first.LastSeenAt) {
		t.Fatal("last_seen_at moved backwards")
	}
}

// C04 acceptance: upsert SQL A/inverse A, approve A, then SQL B/inverse
// B. Every read returns B/B and the approval of A is invalidated.
func TestC04NewRevisionInvalidatesApproval(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	p := proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)
	a := mustPropose(t, ctx, s, p).Recommendation
	approved, err := s.Approve(ctx, a.ID, a.ContentHash, "user:7")
	if err != nil || approved.State != StateApproved || approved.ApprovedHash != a.ContentHash {
		t.Fatalf("approve A: %+v, %v", approved, err)
	}
	p.ForwardSQL, p.InverseSQL = sqlA2, inverseB
	res := mustPropose(t, ctx, s, p)
	b := res.Recommendation
	if res.Outcome != OutcomeRevised || b.ID != a.ID || b.Revision != 2 {
		t.Fatalf("revise = %s id=%d rev=%d, want revised same id rev 2",
			res.Outcome, b.ID, b.Revision)
	}
	if b.State != StateProposed || b.ApprovedHash != "" || b.ApprovedBy != "" ||
		b.ApprovedRevision != nil {
		t.Fatalf("approval survived a new revision: %+v", b)
	}
	revs, _ := s.Revisions(ctx, b.ID)
	if len(revs) != 2 || revs[1].ForwardSQL != sqlA2 || revs[1].InverseSQL != inverseB ||
		revs[0].ForwardSQL != sqlA || revs[0].InverseSQL != inverseA {
		t.Fatalf("revisions = %+v, want A/A then B/B", revs)
	}
	if _, err := s.Approve(ctx, b.ID, a.ContentHash, "user:7"); !errors.Is(err, ErrRevised) {
		t.Fatalf("re-approving stale content A: err=%v, want ErrRevised", err)
	}
	last := lastTransition(t, ctx, s, b.ID)
	if last.From != StateApproved || last.To != StateProposed || last.Revision != 2 {
		t.Fatalf("history tail = %+v, want approved→proposed at revision 2", last)
	}
	again, err := s.Approve(ctx, b.ID, b.ContentHash, "user:7")
	if err != nil || again.ApprovedRevision == nil || *again.ApprovedRevision != 2 {
		t.Fatalf("approving B: %+v, %v", again, err)
	}
}

func TestReviseSupersedesQueuedApprovalsOfOldContent(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	p := proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)
	a := mustPropose(t, ctx, s, p).Recommendation
	var queueID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_queue
		(finding_id, proposed_sql, rollback_sql, action_risk, recommendation_id,
		 recommendation_revision, content_hash)
		VALUES ($1, $2, $3, 'safe', $4, 1, $5) RETURNING id`,
		*a.FindingID, sqlA, inverseA, a.ID, a.ContentHash).Scan(&queueID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	p.ForwardSQL = sqlA2
	mustPropose(t, ctx, s, p)
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM sage.action_queue WHERE id=$1`,
		queueID).Scan(&status); err != nil || status != "superseded" {
		t.Fatalf("queued approval of old content: status=%q err=%v, want superseded",
			status, err)
	}
}

func TestProposeHoldsInFlightRecommendations(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	for _, state := range []State{StateApplying, StateApplied, StateVerifying} {
		p := proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)
		rec := mustPropose(t, ctx, s, p).Recommendation
		setState(t, ctx, pool, rec.ID, state)
		p.ForwardSQL = sqlA2
		res := mustPropose(t, ctx, s, p)
		got := mustGet(t, ctx, s, rec.ID)
		if res.Outcome != OutcomeHeld || got.State != state || got.Revision != 1 {
			t.Errorf("%s: outcome=%s state=%s rev=%d, want held unchanged",
				state, res.Outcome, got.State, got.Revision)
		}
	}
}

func TestProposeAfterTerminal(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	cases := []struct {
		state      State
		sameHashOK bool
	}{
		{StateVerified, true}, {StateInconclusive, true}, {StateSuperseded, true},
		{StateReverted, false}, {StateAbandoned, false},
	}
	for _, tc := range cases {
		p := proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)
		old := mustPropose(t, ctx, s, p).Recommendation
		setState(t, ctx, pool, old.ID, tc.state)
		res := mustPropose(t, ctx, s, p)
		if tc.sameHashOK != (res.Outcome == OutcomeCreated) {
			t.Errorf("%s same content: outcome %s", tc.state, res.Outcome)
		}
		if tc.sameHashOK && res.Recommendation.ID == old.ID {
			t.Errorf("%s: reused the terminal head", tc.state)
		}
		p.ForwardSQL = sqlA2
		if res := mustPropose(t, ctx, s, p); res.Outcome != OutcomeCreated &&
			!tc.sameHashOK {
			t.Errorf("%s changed content: outcome %s, want created", tc.state, res.Outcome)
		}
	}
}

func TestTwoIndexCandidatesPersistIndependently(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	p := proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)
	q := p
	q.ForwardSQL = "CREATE INDEX CONCURRENTLY idx_b ON public.orders (b)"
	q.InverseSQL = inverseB
	a := mustPropose(t, ctx, s, p).Recommendation
	b := mustPropose(t, ctx, s, q).Recommendation
	if a.ID == b.ID {
		t.Fatal("C05: second candidate overwrote the first")
	}
	ra, _ := s.Revisions(ctx, a.ID)
	rb, _ := s.Revisions(ctx, b.ID)
	if ra[0].InverseSQL != inverseA || rb[0].InverseSQL != inverseB {
		t.Fatalf("inverse SQL crossed between candidates: %q %q",
			ra[0].InverseSQL, rb[0].InverseSQL)
	}
}

func TestRevisionsAndHistoryAreImmutable(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := mustPropose(t, ctx, s, proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)).
		Recommendation
	if _, err := pool.Exec(ctx, `UPDATE sage.recommendation_revision
		SET forward_sql = 'DROP TABLE x' WHERE recommendation_id = $1`, rec.ID); err == nil {
		t.Fatal("a revision was rewritten in place")
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.recommendation_transition
		SET to_state = 'verified' WHERE recommendation_id = $1`, rec.ID); err == nil {
		t.Fatal("transition history was rewritten in place")
	}
	revs, _ := s.Revisions(ctx, rec.ID)
	if revs[0].ForwardSQL != sqlA {
		t.Fatalf("forward SQL = %q after refused update", revs[0].ForwardSQL)
	}
}

func TestProposeRejectsInvalidInput(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	for name, p := range map[string]Proposal{
		"no forward SQL": {DatabaseName: "d", Category: "c", Target: "t"},
		"no category":    {DatabaseName: "d", Target: "t", ForwardSQL: sqlA},
		"no target":      {DatabaseName: "d", Category: "c", ForwardSQL: sqlA},
	} {
		if _, err := s.Propose(ctx, p); err == nil {
			t.Errorf("%s: Propose accepted an invalid proposal", name)
		}
	}
}

func TestApproveRequiresProposedState(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := mustPropose(t, ctx, s, proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)).
		Recommendation
	setState(t, ctx, pool, rec.ID, StateVerifying)
	if _, err := s.Approve(ctx, rec.ID, rec.ContentHash, "user:1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving a verifying recommendation: err=%v, want ErrConflict", err)
	}
	if _, err := s.Approve(ctx, 0, rec.ContentHash, "user:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approving id 0: err=%v, want ErrNotFound", err)
	}
}

// A proposal with no open finding behind it (just resolved by an action,
// or suppressed) creates nothing: there is nothing to act on.
func TestProposeWithoutOpenFindingCreatesNothing(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	db := uniqueDB(t)
	p := Proposal{DatabaseName: db, Category: "no_finding_" + db, Target: "public.x",
		ForwardSQL: sqlA}
	res, err := s.Propose(ctx, p)
	if err != nil || res.Outcome != OutcomeNoFinding || res.Recommendation.ID != 0 {
		t.Fatalf("propose without finding = %+v, %v; want no_open_finding", res, err)
	}
	if list, _ := s.List(ctx, ListFilter{DatabaseName: db}); len(list) != 0 {
		t.Fatalf("%d recommendations created without a finding", len(list))
	}
}
