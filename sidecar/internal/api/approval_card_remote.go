package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// An approval card approved with an API token (the CLI, cli-01a) is a
// remote approval: like one from chat it is capped by
// notification_policy.chat_approval_max_tier (ux-09a, D7), and a high-risk
// change is approved in pg_sage only. The check is here, on the server, so
// a modified client cannot skip it. A signed-in approval is not capped.

// remoteApprovalAllowed refuses a token approval of queue item id above
// the cap with the app link (403 remote_approval_capped), answering
// itself; a session request is always allowed.
func (h *approvalCardHandlers) remoteApprovalAllowed(w http.ResponseWriter, r *http.Request,
	inst *fleet.DatabaseInstance, id int) bool {
	if _, token := mcp.PrincipalFromContext(r.Context()); !token {
		return true
	}
	a, err := store.NewActionStore(inst.Pool).GetByID(r.Context(), id)
	if err != nil {
		internalError(w, r, "approval card tier", err)
		return false
	}
	allowed, reason := approvalcard.RemoteApprovalAllowed(approvalcard.ActionTier(*a),
		h.remoteCap)
	if allowed {
		return true
	}
	slog.Info("token approval above the remote approval cap refused", "queue_id", id,
		"database", inst.Name, "reason", reason)
	sreErrorCode(w, "approve this change in pg_sage: "+approvalcard.ApprovalLink(id)+
		" ("+reason+")", "remote_approval_capped", http.StatusForbidden)
	return false
}

// cardSchedule is a scheduled item's window: when it may start (also as a
// label in UTC), when the window closes, and whether its start has come
// by the server's clock.
func cardSchedule(a store.QueuedAction, now time.Time) map[string]any {
	m := map[string]any{"due": false}
	addScheduleFields(m, a)
	if a.RunAfter != nil {
		m["run_after_label"] = policy.FormatWindowStart(*a.RunAfter, time.UTC)
		m["due"] = !now.Before(*a.RunAfter)
	}
	return m
}
