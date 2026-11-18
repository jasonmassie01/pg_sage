package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/store"
)

// Bulk approve (ux-07b): one request approves up to bulkApproveMax
// safe-tier approval cards. It is not all-or-nothing and it adds no write
// path: each item is decided in turn exactly as the card's own approve
// route decides it (the card hash the person saw, the card still pending,
// approveAndRunExpecting bound to the card's SQL, so the policy gate,
// trust, change lease, verification and audit run per item). An item
// that is not safe tier on its card is refused, never approved. The
// answer has one result per item, in the request's order.

const (
	bulkApprovePath = "/api/v1/approvals/bulk-approve"
	bulkApproveMax  = 50
	// safeTier is the approval card risk tier bulk approve may take.
	safeTier = "safe"
)

// bulkApproveItem is one card the caller selected.
type bulkApproveItem struct {
	ID       int    `json:"id"`
	Database string `json:"database"`
	CardHash string `json:"card_hash"`
}

// bulkApproveResult is what happened to one item: approved (ran),
// scheduled (for the next window), failed (approved, its SQL failed) or
// refused (not approved; Code and Reason say why).
type bulkApproveResult struct {
	ID                 int    `json:"id"`
	Database           string `json:"database"`
	Status             string `json:"status"`
	Executed           bool   `json:"executed"`
	Code               string `json:"code,omitempty"`
	Reason             string `json:"reason,omitempty"`
	ActionLogID        any    `json:"action_log_id,omitempty"`
	VerificationStatus string `json:"verification_status,omitempty"`
	RunAfter           any    `json:"run_after,omitempty"`
	RunAfterLabel      string `json:"run_after_label,omitempty"`
}

func (h *approvalCardHandlers) bulkApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []bulkApproveItem `json:"items"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&req); err != nil {
		sreErrorCode(w, "invalid JSON body", "invalid_request", http.StatusBadRequest)
		return
	}
	if msg := h.validateBulk(req.Items); msg != "" {
		sreErrorCode(w, msg, "invalid_request", http.StatusBadRequest)
		return
	}
	user := UserFromContext(r.Context())
	results := make([]bulkApproveResult, 0, len(req.Items))
	counts := map[string]int{"approved": 0, "scheduled": 0, "refused": 0, "failed": 0}
	for _, item := range req.Items {
		res := h.approveOne(r, item, user.ID)
		counts[res.Status]++
		results = append(results, res)
	}
	jsonResponse(w, map[string]any{"results": results, "approved": counts["approved"],
		"scheduled": counts["scheduled"], "refused": counts["refused"],
		"failed": counts["failed"]})
}

// validateBulk checks the whole request before any item is read; it names
// the first problem, or "" when the request may run. It fills in the
// database of an item that omits it when the fleet has one database.
func (h *approvalCardHandlers) validateBulk(items []bulkApproveItem) string {
	switch {
	case len(items) == 0:
		return "bulk approve needs at least one item"
	case len(items) > bulkApproveMax:
		return fmt.Sprintf("bulk approve takes at most %d items; got %d", bulkApproveMax,
			len(items))
	}
	only := ""
	if instances := sortedFleetInstances(h.mgr); len(instances) == 1 {
		only = instances[0].Name
	}
	seen := make(map[string]bool, len(items))
	for i := range items {
		it := &items[i]
		it.Database = strings.TrimSpace(it.Database)
		if it.Database == "" || it.Database == "all" {
			it.Database = only
		}
		switch {
		case it.ID <= 0:
			return fmt.Sprintf("item %d: id must be a positive queue item id", i+1)
		case strings.TrimSpace(it.CardHash) == "":
			return fmt.Sprintf("item %d: card_hash is required", i+1)
		case it.Database == "":
			return fmt.Sprintf("item %d: database is required", i+1)
		}
		key := fmt.Sprintf("%s\x00%d", it.Database, it.ID)
		if seen[key] {
			return fmt.Sprintf("duplicate item %d on database %q", it.ID, it.Database)
		}
		seen[key] = true
	}
	return ""
}

// refuse is a result for an item that was not approved.
func refuse(item bulkApproveItem, code, reason string) bulkApproveResult {
	return bulkApproveResult{ID: item.ID, Database: item.Database, Status: "refused",
		Code: code, Reason: reason}
}

// approveOne decides one item as the card approve route does.
func (h *approvalCardHandlers) approveOne(r *http.Request, item bulkApproveItem,
	userID int) bulkApproveResult {
	ctx := r.Context()
	if err := ctx.Err(); err != nil {
		return refuse(item, "cancelled", "the request ended before this item was decided")
	}
	inst := h.mgr.GetInstance(item.Database)
	if inst == nil || inst.Pool == nil || inst.Executor == nil {
		return refuse(item, "not_found", "database not found")
	}
	c, res, ok := h.bulkCard(ctx, inst, item)
	if !ok {
		return res
	}
	as := store.NewActionStore(inst.Pool)
	body, refused := approveAndRunExpecting(ctx, as, inst.Executor, item.ID, userID, c.SQL)
	if refused != nil {
		return refusedResult(ctx, as, item, refused)
	}
	out := bulkOutcome(item, body)
	if out.Status == "approved" || out.Status == "scheduled" {
		recordDecision(r, inbox.KindChangeApproval, c.ProposedAt)
	}
	return out
}

// bulkCard loads the item's card and checks it may be bulk approved: it
// exists, still waits, shows the hash the caller saw, and is safe tier.
func (h *approvalCardHandlers) bulkCard(ctx context.Context, inst *fleet.DatabaseInstance,
	item bulkApproveItem) (approvalcard.Card, bulkApproveResult, bool) {
	c, err := h.cardLoader(inst).Card(ctx, item.ID)
	switch {
	case errors.Is(err, approvalcard.ErrNotFound):
		return c, refuse(item, "not_found", "queue item not found"), false
	case errors.Is(err, approvalcard.ErrPlanStepItem):
		return c, refuse(item, "plan_step", err.Error()), false
	case err != nil:
		slog.Error("bulk approve: approval card failed", "database", item.Database,
			"queue_id", item.ID, "error", err)
		return c, refuse(item, "internal", "the approval card could not be read"), false
	}
	switch {
	case c.Status != "pending":
		return c, refuse(item, "not_pending",
			"the action is no longer awaiting approval ("+c.Status+")"), false
	case item.CardHash != c.CardHash:
		return c, refuse(item, "content_changed",
			"the action changed after this card was shown; review it again"), false
	case !strings.EqualFold(strings.TrimSpace(c.Risk.Tier), safeTier):
		return c, refuse(item, "not_safe_tier", "bulk approve is safe tier only; this "+
			"card is "+c.Risk.Tier+" risk: approve it on its own card"), false
	}
	return c, bulkApproveResult{}, true
}

// refusedResult maps approveAndRunExpecting's refusal onto a result. A
// refusal of an item a person decided meanwhile (another tab, a chat
// button) says so; a gate refusal keeps the gate's reason.
func refusedResult(ctx context.Context, as *store.ActionStore, item bulkApproveItem,
	refused *approvalRefusal) bulkApproveResult {
	if refused.approveErr != nil {
		slog.Warn("bulk approve refused", "database", item.Database, "queue_id", item.ID,
			"error", refused.approveErr)
		return refuse(item, "not_pending",
			"the action is no longer awaiting approval, or it changed")
	}
	if a, err := as.GetByID(ctx, item.ID); err == nil && a.DecidedBy != nil {
		return refuse(item, "not_pending",
			"the action is no longer awaiting approval ("+a.Status+")")
	}
	if refused.status == http.StatusNotFound {
		return refuse(item, "not_found", "queue item not found")
	}
	return refuse(item, "not_eligible", refused.msg)
}

// bulkOutcome turns the approve path's answer into a result.
func bulkOutcome(item bulkApproveItem, body map[string]any) bulkApproveResult {
	out := bulkApproveResult{ID: item.ID, Database: item.Database}
	status, _ := body["status"].(string)
	out.Executed, _ = body["executed"].(bool)
	switch status {
	case "approved":
		out.Status = "approved"
		out.ActionLogID = body["action_log_id"]
		out.VerificationStatus, _ = body["verification_status"].(string)
	case "scheduled":
		out.Status = "scheduled"
		out.RunAfter = body["run_after"]
		out.RunAfterLabel, _ = body["run_after_label"].(string)
	default:
		out.Status = "failed"
		out.Code = "execution_failed"
		out.Reason, _ = body["error"].(string)
	}
	return out
}
