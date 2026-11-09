package approvalcard

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/store"
)

// RevertRow is the action a "Revert action N" item reverts (policy-05):
// its outcome and why pg_sage withheld the automatic revert.
type RevertRow struct {
	ActionID int64
	Outcome  string
	Reason   string
}

// revert reads the reverted action of a revert item (nil for any other).
func (l Loader) revert(ctx context.Context, a store.QueuedAction) (*RevertRow, error) {
	if a.RevertsActionID == nil {
		return nil, nil
	}
	r := RevertRow{ActionID: *a.RevertsActionID}
	err := l.Pool.QueryRow(ctx, `/* pg_sage */ SELECT outcome, COALESCE(rollback_reason, '')
		FROM sage.action_log WHERE id = $1`, r.ActionID).Scan(&r.Outcome, &r.Reason)
	return optional("reverted action", &r, err)
}

// revertReasons says why a revert item waits for a person: pg_sage
// withheld the automatic revert, and why. After an emergency stop it is
// approvable once the stop is lifted.
func revertReasons(r *RevertRow) []Reason {
	if r == nil {
		return nil
	}
	reason := strings.TrimSpace(r.Reason)
	if reason == "" {
		reason = "no reason was recorded"
	}
	text := fmt.Sprintf("Action %d regressed and pg_sage withheld its automatic revert: %s",
		r.ActionID, reason)
	if strings.Contains(strings.ToLower(reason), "emergency stop") ||
		strings.Contains(reason, "emergency_stop") {
		text += "; approve the revert after the stop is lifted"
	}
	return []Reason{{Code: "revert_withheld", Text: text}}
}
