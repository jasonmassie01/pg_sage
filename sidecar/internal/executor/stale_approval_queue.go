package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// queuedApproval is one sage.action_queue proposal for a finding, its
// recommendation or its SQL.
type queuedApproval struct {
	ID               int
	Status           string
	Expired          bool
	RecommendationID *int64
	ContentHash      string
	SQL              string
}

// sameContent reports whether the proposal approves exactly sql: a pinned
// proposal by its recommendation and content hash, a legacy one by its SQL.
func (q queuedApproval) sameContent(sql string, cand *recommendation.Candidate) bool {
	proposed := strings.TrimSpace(q.SQL)
	if proposed == "" || proposed != strings.TrimSpace(sql) {
		return false
	}
	if q.RecommendationID == nil || cand == nil {
		return true
	}
	return *q.RecommendationID == cand.ID && q.ContentHash == cand.ContentHash
}

// rowsQuerier is a pool or a transaction.
type rowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

const queuedApprovalsSQL = `/* pg_sage */ SELECT id, status, expires_at <= now(),
	recommendation_id, COALESCE(content_hash, ''), proposed_sql
	FROM sage.action_queue
	WHERE status = ANY($4)
	  AND (finding_id = $1 OR recommendation_id = $2 OR proposed_sql = $3)
	ORDER BY id`

// loadQueuedApprovals reads the proposals in statuses for the finding, the
// candidate's recommendation or sql; forUpdate locks them.
func loadQueuedApprovals(
	ctx context.Context, q rowsQuerier, statuses []string, findingID int64, sql string,
	cand *recommendation.Candidate, forUpdate bool,
) ([]queuedApproval, error) {
	var recID int64
	if cand != nil {
		recID = cand.ID
	}
	query := queuedApprovalsSQL
	if forUpdate {
		query += " FOR UPDATE"
	}
	rows, err := q.Query(ctx, query, findingID, recID, sql, statuses)
	if err != nil {
		return nil, fmt.Errorf("read queued proposals: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (queuedApproval, error) {
		var a queuedApproval
		err := row.Scan(&a.ID, &a.Status, &a.Expired, &a.RecommendationID,
			&a.ContentHash, &a.SQL)
		return a, err
	})
}

// classifyQueuedApprovals decides what an authorized change does about the
// live proposals around it: the unexpired pending proposals of exactly its
// content are superseded; an approved (or approved and failed) proposal,
// an operator rejection of this content, or a pending proposal of other
// content blocks it. Expired and other-content rejections are ignored.
func classifyQueuedApprovals(
	rows []queuedApproval, sql string, cand *recommendation.Candidate,
) (supersede []int, blocked string) {
	for _, q := range rows {
		same := q.sameContent(sql, cand)
		switch {
		case q.Status == "approved" || q.Status == "failed":
			return nil, fmt.Sprintf("operator-approved proposal %d owns this change", q.ID)
		case q.Status == "rejected" && same:
			return nil, fmt.Sprintf("an operator rejected this exact change (queue item %d)",
				q.ID)
		case q.Status != "pending" || q.Expired:
			continue
		case !same:
			return nil, fmt.Sprintf("pending proposal %d is for different content", q.ID)
		}
		supersede = append(supersede, q.ID)
	}
	return supersede, ""
}

// retireStaleApprovals runs under the change lease, after the standing
// gate re-authorized the change and right before it runs. It supersedes
// the pending proposals the authorization made unnecessary and reports
// whether the change may run: not when an operator decision owns it, its
// content changed, another worker already took the recommendation, or the
// proposals cannot be read (fail closed).
func (e *Executor) retireStaleApprovals(
	ctx context.Context, f analyzer.Finding, findingID, decisionID int64,
	cand *recommendation.Candidate,
) bool {
	if e.pool == nil {
		return true
	}
	proceed := false
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		rows, err := loadQueuedApprovals(ctx, tx,
			[]string{"pending", "approved", "failed", "rejected"},
			findingID, f.RecommendedSQL, cand, true)
		if err != nil {
			return err
		}
		ids, blocked := classifyQueuedApprovals(rows, f.RecommendedSQL, cand)
		if blocked != "" {
			e.logFn("executor", "not running %q: %s", f.Title, blocked)
			return nil
		}
		if moved, err := candidateMoved(ctx, tx, cand); err != nil || moved {
			if moved {
				e.logFn("executor", "not running %q: its recommendation moved on", f.Title)
			}
			return err
		}
		proceed = true
		return e.supersedeQueued(ctx, tx, f, ids, staleApprovalReason(f, cand, decisionID))
	})
	if err != nil {
		e.logFn("executor", "not running %q: check queued proposals: %v", f.Title, err)
		return false
	}
	return proceed
}

// candidateMoved reports a recommendation that changed since this cycle
// read it: another worker claimed or ran it, or it was revised.
func candidateMoved(ctx context.Context, tx pgx.Tx, cand *recommendation.Candidate) (
	bool, error) {
	if cand == nil {
		return false, nil
	}
	var state string
	var revision int
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT state, revision
		FROM sage.recommendation WHERE id = $1`, cand.ID).Scan(&state, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("re-read recommendation %d: %w", cand.ID, err)
	}
	return recommendation.State(state) != cand.State || revision != cand.Revision, nil
}

func (e *Executor) supersedeQueued(
	ctx context.Context, tx pgx.Tx, f analyzer.Finding, ids []int, reason string,
) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.action_queue
		SET status = 'superseded', reason = $2
		WHERE id = ANY($1) AND status = 'pending'`, ids, reason); err != nil {
		return fmt.Errorf("supersede queued proposals %v: %w", ids, err)
	}
	e.logFn("executor", "superseded queued proposals %v for %q: %s", ids, f.Title, reason)
	return nil
}
