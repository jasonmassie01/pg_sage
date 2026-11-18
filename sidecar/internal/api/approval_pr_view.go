package api

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/gitops"
	"github.com/pg-sage/sidecar/internal/store"
)

// What the app shows of approval PRs (cli-02b): the inbox card names the
// change's PR (or why it has none: a tier above the remote approval cap
// stays app-only, or the open failed), and GET /api/v1/gitops carries PR
// mode's state for the Settings banner (off after a refused token).

var approvalPRView struct {
	mu   sync.Mutex
	prs  *gitops.ApprovalPRs
	pool *pgxpool.Pool
}

// SetApprovalPRs installs the approval PR runner and the control database
// it records PRs in (nil: approval PRs are off).
func SetApprovalPRs(prs *gitops.ApprovalPRs, pool *pgxpool.Pool) {
	approvalPRView.mu.Lock()
	defer approvalPRView.mu.Unlock()
	approvalPRView.prs, approvalPRView.pool = prs, pool
}

func currentApprovalPRs() (*gitops.ApprovalPRs, *pgxpool.Pool) {
	approvalPRView.mu.Lock()
	defer approvalPRView.mu.Unlock()
	if approvalPRView.prs == nil || approvalPRView.prs.Status().Repo == "" {
		return nil, nil
	}
	return approvalPRView.prs, approvalPRView.pool
}

// approvalPRStatus is PR mode's state, or nil when it is not configured.
func approvalPRStatus() *gitops.ApprovalPRStatus {
	prs, _ := currentApprovalPRs()
	if prs == nil {
		return nil
	}
	st := prs.Status()
	return &st
}

// approvalPRNote is the inbox's PR line of a card, or nil.
func approvalPRNote(ctx context.Context, database string, c approvalcard.Card,
	a *store.QueuedAction) map[string]any {
	prs, pool := currentApprovalPRs()
	if prs == nil {
		return nil
	}
	rec, err := gitops.LookupApprovalPR(ctx, pool, database, c.QueueID)
	if err != nil {
		slog.Warn("approval PR not read for the inbox", "database", database,
			"queue_id", c.QueueID, "error", err)
		return nil
	}
	if rec != nil {
		return map[string]any{"state": rec.State, "pr_url": rec.PRURL,
			"pr_number": rec.PRNumber, "repo": rec.Repo, "detail": rec.Detail}
	}
	if a == nil || a.Status != "pending" {
		return nil
	}
	if ok, reason := approvalcard.RemoteApprovalAllowed(cappedTier(*a, c),
		prs.Config.ChatCap); !ok {
		return map[string]any{"state": "app_only",
			"detail": "Not opened as a pull request: " + reason + "; approve it here."}
	}
	if st := prs.Status(); st.Disabled != "" {
		return map[string]any{"state": "disabled", "detail": st.Disabled}
	}
	return nil
}
