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
	a *store.QueuedAction) *approvalcard.PRNote {
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
		return &approvalcard.PRNote{State: rec.State, Repo: rec.Repo, PRNumber: rec.PRNumber,
			PRURL: rec.PRURL, Detail: rec.Detail}
	}
	if a == nil || a.Status != "pending" {
		return nil
	}
	if ok, reason := approvalcard.RemoteApprovalAllowed(cappedTier(*a, c),
		prs.Config.ChatCap); !ok {
		return &approvalcard.PRNote{State: "app_only",
			Detail: "Not opened as a pull request: " + reason + "; approve it here."}
	}
	if st := prs.Status(); st.Disabled != "" {
		return &approvalcard.PRNote{State: "disabled", Detail: st.Disabled}
	}
	return nil
}

// withApprovalPRNotes sets each pending card's PR note; the tier comes
// from the card's own risk, SQL and action type, as cappedTier reads them.
func withApprovalPRNotes(ctx context.Context, cards []approvalcard.Card) {
	if prs, _ := currentApprovalPRs(); prs == nil {
		return
	}
	for i := range cards {
		c := cards[i]
		a := store.QueuedAction{ID: c.QueueID, Status: c.Status, ActionRisk: c.Risk.Tier,
			ProposedSQL: c.SQL, ActionType: c.ActionType}
		cards[i].ApprovalPR = approvalPRNote(ctx, c.Database, c, &a)
	}
}
