package approvalcard

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Outcome is where a queue item ended: Final once there is nothing left to
// wait for (verified, rolled back, regressed, failed, or closed without
// running), with the numbers the verification recorded.
type Outcome struct {
	Final   bool
	Verdict string
	Detail  string
}

// logVerdicts maps a terminal action_log outcome to the card verdict;
// other outcomes (pending, monitoring, rolling back) are still running.
var logVerdicts = map[string]string{
	"success":          "verified",
	"rolled_back":      "rolled_back",
	"rollback_failed":  "regressed",
	"failed":           "failed",
	"unverifiable":     "unverifiable",
	"rollback_skipped": "regressed",
}

// closedStatuses are queue statuses that end an item without a run.
var closedStatuses = map[string]bool{"rejected": true, "expired": true,
	"superseded": true, "failed": true, "blocked": true}

// ReadOutcome reads a queue item's outcome from its executed action and
// that action's verification.
func ReadOutcome(ctx context.Context, pool *pgxpool.Pool, queueID int) (Outcome, error) {
	var status, reason, rollbackReason, verifyReason string
	var logOutcome, verifyVerdict *string
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT q.status, COALESCE(q.reason, ''),
		al.outcome, COALESCE(al.rollback_reason, ''), v.verdict, COALESCE(v.reason, '')
		FROM sage.action_queue q
		LEFT JOIN sage.action_log al ON al.id = q.action_log_id
		LEFT JOIN sage.verification v ON v.id = al.verification_id
		WHERE q.id = $1`, queueID).Scan(&status, &reason, &logOutcome, &rollbackReason,
		&verifyVerdict, &verifyReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, fmt.Errorf("%w: %d", ErrNotFound, queueID)
	}
	if err != nil {
		return Outcome{}, fmt.Errorf("approvalcard: read outcome of %d: %w", queueID, err)
	}
	if logOutcome != nil {
		verdict, final := logVerdicts[*logOutcome]
		if !final {
			return Outcome{Verdict: "pending"}, nil
		}
		return Outcome{Final: true, Verdict: verdict,
			Detail: joinNonEmpty(verifyReason, rollbackReason)}, nil
	}
	if closedStatuses[status] {
		return Outcome{Final: true, Verdict: status, Detail: reason}, nil
	}
	return Outcome{Verdict: "pending"}, nil
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}
