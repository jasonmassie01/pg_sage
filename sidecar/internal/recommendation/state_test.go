package recommendation

import (
	"errors"
	"testing"
)

// legalEdges is the specification, written independently of the
// implementation: proposed → approved → applying → applied → verifying →
// verified | reverted | inconclusive, plus superseded, failed, abandoned.
// Entering proposed is never a transition: only a new revision does it.
var legalEdges = map[State][]State{
	StateProposed:  {StateApproved, StateSuperseded},
	StateApproved:  {StateApplying, StateSuperseded},
	StateApplying:  {StateApplied, StateFailed},
	StateApplied:   {StateVerifying},
	StateVerifying: {StateVerified, StateReverted, StateInconclusive},
	StateFailed:    {StateApplying, StateAbandoned, StateSuperseded},
}

func isLegalEdge(from, to State) bool {
	for _, s := range legalEdges[from] {
		if s == to {
			return true
		}
	}
	return false
}

func TestCanTransitionMatrix(t *testing.T) {
	legal := 0
	for _, from := range AllStates() {
		for _, to := range AllStates() {
			want := isLegalEdge(from, to)
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
			if want {
				legal++
			}
		}
	}
	if legal != 13 {
		t.Fatalf("specification has %d legal edges, want 13", legal)
	}
}

func TestCanTransitionRejectsUnknownStates(t *testing.T) {
	for _, pair := range [][2]State{
		{"bogus", StateApproved}, {StateProposed, "bogus"}, {"", ""},
	} {
		if CanTransition(pair[0], pair[1]) {
			t.Errorf("CanTransition(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

func TestNothingTransitionsIntoProposed(t *testing.T) {
	for _, from := range AllStates() {
		if CanTransition(from, StateProposed) {
			t.Errorf("CanTransition(%s, proposed) = true; only a revision re-proposes", from)
		}
	}
}

func TestTerminalAndValidStates(t *testing.T) {
	terminal := map[State]bool{
		StateVerified: true, StateReverted: true, StateInconclusive: true,
		StateSuperseded: true, StateAbandoned: true,
	}
	for _, s := range AllStates() {
		if !s.Valid() {
			t.Errorf("%s.Valid() = false", s)
		}
		if s.Terminal() != terminal[s] {
			t.Errorf("%s.Terminal() = %v, want %v", s, s.Terminal(), terminal[s])
		}
		if s.Terminal() && len(legalEdges[s]) != 0 {
			t.Errorf("terminal state %s has outgoing edges", s)
		}
	}
	if State("bogus").Valid() || State("").Valid() {
		t.Error("unknown states must be invalid")
	}
	if len(AllStates()) != 11 {
		t.Fatalf("AllStates() has %d states, want 11", len(AllStates()))
	}
}

func TestCanRevise(t *testing.T) {
	revisable := map[State]bool{StateProposed: true, StateApproved: true, StateFailed: true}
	for _, s := range AllStates() {
		if CanRevise(s) != revisable[s] {
			t.Errorf("CanRevise(%s) = %v, want %v", s, CanRevise(s), revisable[s])
		}
	}
}

// TestTransitionEveryPairAgainstPostgres drives every (from, to) pair
// through the durable compare-and-set: legal pairs move the row and
// record history; illegal pairs fail with ErrIllegalTransition and leave
// the row untouched.
func TestTransitionEveryPairAgainstPostgres(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	for _, from := range AllStates() {
		for _, to := range AllStates() {
			db := uniqueDB(t)
			p := proposal(t, ctx, pool, db, "CREATE INDEX CONCURRENTLY i ON t (a)",
				"DROP INDEX CONCURRENTLY i")
			rec := mustPropose(t, ctx, s, p).Recommendation
			setState(t, ctx, pool, rec.ID, from)
			before := len(transitionsOf(t, ctx, s, rec.ID))
			err := s.Transition(ctx, rec.ID, from, to, rec.Revision, "test", "matrix")
			got := mustGet(t, ctx, s, rec.ID)
			after := len(transitionsOf(t, ctx, s, rec.ID))
			if isLegalEdge(from, to) {
				if err != nil || got.State != to || after != before+1 {
					t.Errorf("%s→%s: err=%v state=%s history %d→%d, want moved",
						from, to, err, got.State, before, after)
				}
				continue
			}
			if !errors.Is(err, ErrIllegalTransition) || got.State != from || after != before {
				t.Errorf("%s→%s: err=%v state=%s history %d→%d, want refused",
					from, to, err, got.State, before, after)
			}
		}
	}
}

func TestTransitionCASRejectsStaleStateOrRevision(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	p := proposal(t, ctx, pool, uniqueDB(t), "CREATE INDEX CONCURRENTLY i ON t (a)", "")
	rec := mustPropose(t, ctx, s, p).Recommendation
	setState(t, ctx, pool, rec.ID, StateApproved)

	err := s.Transition(ctx, rec.ID, StateProposed, StateApproved, rec.Revision, "t", "stale")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale from-state: err=%v, want ErrConflict", err)
	}
	err = s.Transition(ctx, rec.ID, StateApproved, StateApplying, rec.Revision+1, "t", "x")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: err=%v, want ErrConflict", err)
	}
	err = s.Transition(ctx, rec.ID+1_000_000, StateApproved, StateApplying, 1, "t", "x")
	if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: err=%v, want ErrConflict or ErrNotFound", err)
	}
	if got := mustGet(t, ctx, s, rec.ID); got.State != StateApproved {
		t.Fatalf("state after refused CAS = %s, want approved", got.State)
	}
}
