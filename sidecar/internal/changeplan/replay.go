package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// Errors ReplayInput returns when the record cannot be trusted.
var (
	ErrReplayHashMismatch = errors.New("changeplan: replay input hash mismatch")
	ErrReplayChain        = errors.New("changeplan: replay input fails its audit chain")
)

// ReplayPass is one stored model pass with the chain link that seals it.
type ReplayPass struct {
	ModelPass
	ChainSeq  int64  `json:"chain_seq"`
	ChainHash string `json:"chain_hash"`
}

// ReplayInput is what a later model is replayed on (review-05 compares
// the answers): every model pass that contributed to the plan, oldest
// first. A plan with no model pass has an empty list.
type ReplayInput struct {
	PlanID int64        `json:"plan_id"`
	Passes []ReplayPass `json:"passes"`
}

// ReplayInput returns plan planID's model passes, each checked against its
// raw_output_sha256 and its chain link, and the chain verified from the
// first of them to its head, so an edited, deleted or unchained pass fails
// the replay instead of being replayed. A plan retention purged still
// replays from its records; ErrNotFound means neither plan nor record.
func (s *Store) ReplayInput(ctx context.Context, planID int64) (ReplayInput, error) {
	in, err := s.replayPasses(ctx, planID)
	if err != nil {
		return ReplayInput{}, fmt.Errorf("changeplan: replay input of plan %d: %w", planID,
			err)
	}
	if len(in.Passes) == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1
			FROM sage.plan WHERE id = $1)`, planID).Scan(&exists); err != nil {
			return ReplayInput{}, fmt.Errorf("changeplan: replay input of plan %d: %w",
				planID, err)
		}
		if !exists {
			return ReplayInput{}, fmt.Errorf("changeplan: replay input: %w: plan %d",
				ErrNotFound, planID)
		}
		return in, nil
	}
	if err := s.checkReplayChain(ctx, in.Passes); err != nil {
		return ReplayInput{}, fmt.Errorf("changeplan: replay input of plan %d: %w", planID,
			err)
	}
	return in, nil
}

// replayPasses reads the passes and checks each one's own hash.
func (s *Store) replayPasses(ctx context.Context, planID int64) (ReplayInput, error) {
	in := ReplayInput{PlanID: planID, Passes: []ReplayPass{}}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT id, plan_id, COALESCE(pack_hash, ''),
		model, prompt_version, raw_output, raw_output_sha256, created_at, chain_seq,
		chain_hash FROM sage.plan_model_record WHERE plan_id = $1 ORDER BY id`, planID)
	if err != nil {
		return in, fmt.Errorf("read the model passes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p ReplayPass
		var seq *int64
		var hash *string
		if err := rows.Scan(&p.ID, &p.PlanID, &p.PackHash, &p.Model, &p.PromptVersion,
			&p.RawOutput, &p.RawOutputSHA256, &p.CreatedAt, &seq, &hash); err != nil {
			return in, fmt.Errorf("read the model passes: %w", err)
		}
		if got := rawOutputSHA256(p.RawOutput); got != p.RawOutputSHA256 {
			return in, fmt.Errorf("%w: model pass %d raw output hashes to %s, recorded %s",
				ErrReplayHashMismatch, p.ID, got, p.RawOutputSHA256)
		}
		if seq == nil || hash == nil {
			return in, fmt.Errorf("%w: model pass %d has no chain link", ErrReplayChain, p.ID)
		}
		p.ChainSeq, p.ChainHash = *seq, *hash
		in.Passes = append(in.Passes, p)
	}
	if err := rows.Err(); err != nil {
		return in, fmt.Errorf("read the model passes: %w", err)
	}
	return in, nil
}

// checkReplayChain requires each pass's own link to be its insert link in
// the chain, and the chain to verify from the lowest of those links (passes
// commit in any id order) to the head.
func (s *Store) checkReplayChain(ctx context.Context, passes []ReplayPass) error {
	spec := auditchain.PlanModelRecords
	first := passes[0].ChainSeq
	for _, p := range passes {
		first = min(first, p.ChainSeq)
		links, err := auditchain.LinksForRow(ctx, s.pool, spec.Chain, p.ID)
		if err != nil {
			return fmt.Errorf("read the chain of model pass %d: %w", p.ID, err)
		}
		if len(links) != 1 || links[0].Op != "I" || links[0].Seq != p.ChainSeq ||
			links[0].Hash != p.ChainHash {
			return fmt.Errorf("%w: model pass %d carries link %d/%s, the chain holds %d "+
				"links for it", ErrReplayChain, p.ID, p.ChainSeq, p.ChainHash, len(links))
		}
	}
	rep, err := auditchain.Verify(ctx, s.pool, spec, auditchain.Window{FromSeq: first})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrReplayChain, err)
	}
	if !rep.OK() {
		kinds := make([]string, 0, len(rep.Problems))
		for _, p := range rep.Problems {
			kinds = append(kinds, fmt.Sprintf("%s (row %d)", p.Kind, p.RowID))
		}
		return fmt.Errorf("%w: %s", ErrReplayChain, strings.Join(kinds, ", "))
	}
	return nil
}
