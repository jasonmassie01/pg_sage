package earned

import "context"

// maxOpenProposals bounds OpenProposals, as PendingProposals is bounded.
const maxOpenProposals = 200

// OpenProposals lists the promotions still waiting for a person, oldest
// first, without expiring anything: it is the inbox's read (ux-01a), and
// a read must not write. A pending proposal already past its expiry is
// left out, so the inbox never offers a decision the ledger would refuse.
func (s *Service) OpenProposals(ctx context.Context) ([]Proposal, error) {
	all, err := s.store.listProposals(ctx, StatusPending, maxOpenProposals)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]Proposal, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- { // listProposals is newest first
		if all[i].ExpiresAt.After(now) {
			out = append(out, all[i])
		}
	}
	return out, nil
}
