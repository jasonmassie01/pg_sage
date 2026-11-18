package policy

import (
	"context"
	"fmt"
)

// MaxOpenProposals bounds OpenProposals.
const MaxOpenProposals = 200

// OpenProposals lists the policy proposals still waiting for
// ratification, oldest first: the inbox's read (ux-01a). limit outside
// 1..MaxOpenProposals is MaxOpenProposals.
func (s *Store) OpenProposals(ctx context.Context, limit int) ([]Proposal, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	if limit < 1 || limit > MaxOpenProposals {
		limit = MaxOpenProposals
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage policy_open_proposals v1 */
SELECT id, database_id, profile, doc, proposed_by, preview, proposed_at,
       COALESCE(proposed_principal_id, ''), COALESCE(proposed_sponsor_id, 0), widening
FROM sage.policy WHERE status = 'proposed' ORDER BY proposed_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("read open policy proposals: %w", err)
	}
	defer rows.Close()
	out := make([]Proposal, 0)
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.Scope.DatabaseID, &p.Profile, &p.Document, &p.Actor,
			&p.Preview, &p.CreatedAt, &p.PrincipalID, &p.SponsorID, &p.Widening); err != nil {
			return nil, fmt.Errorf("scan open policy proposal: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate open policy proposals: %w", err)
	}
	return out, nil
}
