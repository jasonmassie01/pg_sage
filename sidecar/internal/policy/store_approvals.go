package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Approvals of agent policy proposals (spec §6.11, G1-14). An agent
// can only propose; a person ratifies. When the agent's proposal widens
// its base version, two different people must approve it and the agent's
// sponsor is not one of them, unless agents.single_operator_mode lets one
// person approve with a recorded reason, queued for post-hoc review.

// Approval is one person's approval of a policy proposal.
type Approval struct {
	PolicyID       int64     `json:"policy_id"`
	Approver       string    `json:"approver"`
	ApproverUserID int       `json:"approver_user_id"`
	Reason         string    `json:"reason"`
	SingleOperator bool      `json:"single_operator"`
	ReviewStatus   string    `json:"review_status,omitempty"`
	DecidedAt      time.Time `json:"decided_at"`
}

// requiredApprovals is the quorum for a widening agent proposal.
const requiredApprovals = 2

// proposalProvenance is the proposing agent and whether its proposal
// widens current. A base that cannot be read counts as widening.
func proposalProvenance(request ProposalRequest, current Policy) (PrincipalRef, bool) {
	if request.Principal == nil {
		return PrincipalRef{}, false
	}
	widening, err := WidensJSON(current.Document, request.Document)
	return *request.Principal, widening || err != nil
}

// approveProposal records an approval and enforces the quorum. It returns
// nil when the proposal may be activated now.
func approveProposal(ctx context.Context, tx pgx.Tx, proposal Proposal,
	request RatifyRequest) error {
	if proposal.PrincipalID == "" || !proposal.Widening {
		return nil
	}
	if request.ApproverUserID <= 0 {
		return fmt.Errorf("%w: a widening agent proposal needs the approver's user id",
			ErrInvalidDocument)
	}
	reason := strings.TrimSpace(request.Reason)
	if request.SingleOperatorMode {
		if reason == "" {
			return ErrReasonRequired
		}
		return insertApproval(ctx, tx, proposal.ID, request, reason, true)
	}
	if proposal.SponsorID > 0 && request.ApproverUserID == proposal.SponsorID {
		return ErrSponsorCannotApprove
	}
	if err := insertApproval(ctx, tx, proposal.ID, request, reason, false); err != nil {
		return err
	}
	var approvers int
	if err := tx.QueryRow(ctx, `/* pg_sage policy_approval_count v1 */
SELECT count(DISTINCT approver_user_id) FROM sage.policy_approvals
WHERE policy_id = $1 AND approver_user_id <> $2`,
		proposal.ID, proposal.SponsorID).Scan(&approvers); err != nil {
		return fmt.Errorf("count policy approvals: %w", err)
	}
	if approvers < requiredApprovals {
		return ErrSecondApprovalRequired
	}
	return nil
}

func insertApproval(ctx context.Context, tx pgx.Tx, policyID int64,
	request RatifyRequest, reason string, single bool) error {
	review := ""
	if single {
		review = "pending"
	}
	_, err := tx.Exec(ctx, `/* pg_sage policy_approval_insert v1 */
INSERT INTO sage.policy_approvals
    (policy_id, approver, approver_user_id, reason, single_operator, review_status)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
ON CONFLICT (policy_id, approver) DO NOTHING`,
		policyID, strings.TrimSpace(request.Actor), request.ApproverUserID, reason, single,
		review)
	if err != nil {
		return fmt.Errorf("record policy approval: %w", err)
	}
	return nil
}

// Approvals lists a proposal's approvals, oldest first.
func (s *Store) Approvals(ctx context.Context, policyID int64) ([]Approval, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	return s.queryApprovals(ctx, `/* pg_sage policy_approvals_of v1 */
SELECT `+approvalColumns+` FROM sage.policy_approvals
WHERE policy_id = $1 ORDER BY decided_at, approver`, policyID)
}

// PendingReviews is the post-hoc review queue: single-operator approvals
// nobody has reviewed yet, oldest first.
func (s *Store) PendingReviews(ctx context.Context, limit int) ([]Approval, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return s.queryApprovals(ctx, `/* pg_sage policy_approvals_review v1 */
SELECT `+approvalColumns+` FROM sage.policy_approvals
WHERE review_status = 'pending' ORDER BY decided_at LIMIT $1`, limit)
}

const approvalColumns = `policy_id, approver, approver_user_id, reason, single_operator,
       COALESCE(review_status, ''), decided_at`

func (s *Store) queryApprovals(ctx context.Context, sql string, arg any) ([]Approval,
	error) {
	rows, err := s.pool.Query(ctx, sql, arg)
	if err != nil {
		return nil, fmt.Errorf("read policy approvals: %w", err)
	}
	defer rows.Close()
	result := make([]Approval, 0)
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.PolicyID, &a.Approver, &a.ApproverUserID, &a.Reason,
			&a.SingleOperator, &a.ReviewStatus, &a.DecidedAt); err != nil {
			return nil, fmt.Errorf("scan policy approval: %w", err)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policy approvals: %w", err)
	}
	return result, nil
}
