package facts

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ProposalNotifier is told about every new or reopened proposal (the fact
// an operator must now decide) recorded through any Store on its pool: the
// runtime sends the fact card. It owns its errors (it logs them); a failed
// card never fails the proposal.
type ProposalNotifier func(context.Context, Fact)

// SetProposalNotifier makes fn the proposal notifier of every Store over
// pool, whatever built it (the worker, the first look, MCP propose_fact
// and mark_object, Ask Sage), so each proposal path sends one card. A nil
// fn removes the notifier. The returned unset removes this registration
// only if it is still the current one; it is safe to call more than once.
func SetProposalNotifier(pool *pgxpool.Pool, fn ProposalNotifier) (unset func()) {
	st := sharedOf(pool)
	st.mu.Lock()
	st.notifierGen++
	gen := st.notifierGen
	st.notifier = fn
	st.mu.Unlock()
	return func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.notifierGen == gen {
			st.notifier = nil
		}
	}
}

// notifyProposal tells the pool's notifier, if any, about f. It is called
// with no lock held, so the notifier may use the store.
func (s *Store) notifyProposal(ctx context.Context, f Fact) {
	s.shared.mu.Lock()
	fn := s.shared.notifier
	s.shared.mu.Unlock()
	if fn != nil {
		fn(ctx, f)
	}
}
