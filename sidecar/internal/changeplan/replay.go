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
// plan's start (its creation, or its first event once retention purged it)
// to its head, so an edited, deleted, truncated or unchained pass fails
// the replay instead of being left out of it or replayed. ErrNotFound
// means neither plan, event nor record.
func (s *Store) ReplayInput(ctx context.Context, planID int64) (ReplayInput, error) {
	in, err := s.replayPasses(ctx, planID)
	if err != nil {
		return ReplayInput{}, fmt.Errorf("changeplan: replay input of plan %d: %w", planID,
			err)
	}
	from, known, err := s.replayFrom(ctx, planID)
	if err != nil {
		return ReplayInput{}, fmt.Errorf("changeplan: replay input of plan %d: %w", planID,
			err)
	}
	if len(in.Passes) == 0 && !known {
		return ReplayInput{}, fmt.Errorf("changeplan: replay input: %w: plan %d",
			ErrNotFound, planID)
	}
	if err := s.checkReplayChain(ctx, in.Passes, from); err != nil {
		return ReplayInput{}, fmt.Errorf("changeplan: replay input of plan %d: %w", planID,
			err)
	}
	return in, nil
}

// replayFromSQL finds the first plan_model_records link written after the
// plan started (its row's created_at, or its first event's): every pass of
// the plan was linked at or after it, since a pass needs the committed
// plan. It walks back from the head, so it reads the links since the plan.
const replayFromSQL = `/* pg_sage */ WITH s AS (SELECT least(
    (SELECT created_at FROM sage.plan WHERE id = $2),
    (SELECT min(created_at) FROM sage.plan_event WHERE plan_id = $2)) AS at)
SELECT s.at IS NOT NULL, coalesce((SELECT l.seq FROM sage.audit_chain_link l
    WHERE l.chain = $1 AND l.at < s.at ORDER BY l.seq DESC LIMIT 1), 0) + 1
  FROM s`

// replayFrom returns the first link the plan's passes can hold, and
// whether the plan (or its event record) is known; 0 when it is not.
func (s *Store) replayFrom(ctx context.Context, planID int64) (int64, bool, error) {
	var known bool
	var from int64
	if err := s.pool.QueryRow(ctx, replayFromSQL, auditchain.PlanModelRecords.Chain,
		planID).Scan(&known, &from); err != nil {
		return 0, false, fmt.Errorf("find where the plan's model passes start: %w", err)
	}
	if !known {
		return 0, false, nil
	}
	return from, true, nil
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
// the chain, and the chain to verify, without a delete or truncation, from
// from (or the lowest of those links, passes commit in any id order) to the
// head. An empty from with no pass verifies nothing.
func (s *Store) checkReplayChain(ctx context.Context, passes []ReplayPass,
	from int64) error {
	spec := auditchain.PlanModelRecords
	first := from
	for _, p := range passes {
		if first == 0 || p.ChainSeq < first {
			first = p.ChainSeq
		}
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
	if first == 0 {
		return nil
	}
	rep, err := auditchain.Verify(ctx, s.pool, spec, auditchain.Window{FromSeq: first})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrReplayChain, err)
	}
	return replayReportError(rep)
}

// replayReportError fails a report with problems, or with a delete or a
// truncation: the model records are append-only, so either one removed
// passes a replay would otherwise silently leave out.
func replayReportError(rep auditchain.Report) error {
	kinds := make([]string, 0, len(rep.Problems)+2)
	for _, p := range rep.Problems {
		kinds = append(kinds, fmt.Sprintf("%s (row %d)", p.Kind, p.RowID))
	}
	if rep.ProblemsTruncated {
		kinds = append(kinds, "more problems")
	}
	if rep.Deleted > 0 {
		kinds = append(kinds, fmt.Sprintf("%d deleted records", rep.Deleted))
	}
	if rep.Truncations > 0 {
		kinds = append(kinds, fmt.Sprintf("%d truncations", rep.Truncations))
	}
	if len(kinds) == 0 {
		return nil
	}
	return fmt.Errorf("%w: links %d to %d: %s", ErrReplayChain, rep.FromSeq, rep.ToSeq,
		strings.Join(kinds, ", "))
}
