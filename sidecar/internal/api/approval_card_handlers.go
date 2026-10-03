package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// Approval cards (roadmap 1.5): every queued action as one card with the
// why, decided through the same approval path as the Actions page. An
// approval names the card hash it was shown; if the action changed since,
// it is refused.

// Snooze bounds.
const (
	maxSnoozeHours     = 168
	defaultSnoozeHours = 4
)

type approvalCardHandlers struct{ mgr *fleet.DatabaseManager }

func registerApprovalCardRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	operatorUp := RequireRole("admin", "operator")
	h := &approvalCardHandlers{mgr: mgr}
	mux.Handle("GET /api/v1/approvals", operatorUp(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/v1/approvals/{id}", operatorUp(http.HandlerFunc(h.get)))
	mux.Handle("POST /api/v1/approvals/{id}/approve", operatorUp(http.HandlerFunc(h.approve)))
	mux.Handle("POST /api/v1/approvals/{id}/reject", operatorUp(http.HandlerFunc(h.reject)))
	mux.Handle("POST /api/v1/approvals/{id}/snooze", operatorUp(http.HandlerFunc(h.snooze)))
}

// cardLoader reads one database's cards with its executor's trust level.
func cardLoader(inst *fleet.DatabaseInstance) approvalcard.Loader {
	l := approvalcard.Loader{Pool: inst.Pool, Database: inst.Name}
	if inst.Executor != nil {
		l.TrustLevel = inst.Executor.TrustLevel()
	}
	return l
}

func (h *approvalCardHandlers) list(w http.ResponseWriter, r *http.Request) {
	db, ok := readDatabaseParam(w, r)
	if !ok || rejectUnknownDatabase(w, h.mgr, db) {
		return
	}
	cards := make([]approvalcard.Card, 0)
	failures := make([]map[string]string, 0)
	for _, inst := range sortedFleetInstances(h.mgr) {
		if inst == nil || inst.Pool == nil || (db != "" && db != "all" && inst.Name != db) {
			continue
		}
		got, err := cardLoader(inst).Pending(r.Context())
		if err != nil {
			failures = append(failures, fleetReadFailure(inst.Name, "list approval cards", err))
			continue
		}
		cards = append(cards, got...)
	}
	jsonResponse(w, map[string]any{"cards": cards, "total": len(cards), "errors": failures})
}

// cardTarget resolves {id} and ?database= (optional with one database).
func (h *approvalCardHandlers) cardTarget(w http.ResponseWriter,
	r *http.Request) (*fleet.DatabaseInstance, int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		sreErrorCode(w, "invalid queue item id", "invalid_request", http.StatusBadRequest)
		return nil, 0, false
	}
	db, ok := readDatabaseParam(w, r)
	if !ok {
		return nil, 0, false
	}
	instances := sortedFleetInstances(h.mgr)
	if db == "" || db == "all" {
		if len(instances) != 1 {
			sreErrorCode(w, "database is required", "invalid_request", http.StatusBadRequest)
			return nil, 0, false
		}
		db = instances[0].Name
	}
	inst := h.mgr.GetInstance(db)
	if inst == nil || inst.Pool == nil || inst.Executor == nil {
		sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
		return nil, 0, false
	}
	return inst, id, true
}

// loadCard reads the card of a target, answering 404 when it is missing.
func loadCard(w http.ResponseWriter, r *http.Request, inst *fleet.DatabaseInstance,
	id int) (approvalcard.Card, bool) {
	c, err := cardLoader(inst).Card(r.Context(), id)
	switch {
	case errors.Is(err, approvalcard.ErrNotFound):
		sreErrorCode(w, "queue item not found", "not_found", http.StatusNotFound)
		return c, false
	case err != nil:
		internalError(w, r, "approval card", err)
		return c, false
	}
	return c, true
}

func (h *approvalCardHandlers) get(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.cardTarget(w, r)
	if !ok {
		return
	}
	c, ok := loadCard(w, r, inst, id)
	if !ok {
		return
	}
	body := map[string]any{"card": c, "eligible": false}
	if a, err := store.NewActionStore(inst.Pool).GetByID(r.Context(), id); err == nil &&
		a.Status == "pending" {
		readiness := inst.Executor.ApprovalReadiness(*a, time.Now().UTC())
		body["eligible"] = readiness.Eligible
		if readiness.DeferReason != "" {
			body["defer_reason"] = readiness.DeferReason
		}
	}
	jsonResponse(w, body)
}

// decodeCardBody reads a decision body; an empty body is invalid.
func decodeCardBody(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(out); err != nil {
		sreErrorCode(w, "invalid JSON body", "invalid_request", http.StatusBadRequest)
		return false
	}
	return true
}

// pendingCard loads the card of a pending item and checks the hash the
// caller saw (when one is required or given).
func pendingCard(w http.ResponseWriter, r *http.Request, inst *fleet.DatabaseInstance,
	id int, seenHash string, required bool) (approvalcard.Card, bool) {
	if required && strings.TrimSpace(seenHash) == "" {
		sreErrorCode(w, "card_hash is required", "invalid_request", http.StatusBadRequest)
		return approvalcard.Card{}, false
	}
	c, ok := loadCard(w, r, inst, id)
	if !ok {
		return c, false
	}
	if c.Status != "pending" {
		sreErrorCode(w, "the action is no longer awaiting approval ("+c.Status+")",
			"not_pending", http.StatusConflict)
		return c, false
	}
	if seenHash != "" && seenHash != c.CardHash {
		sreErrorCode(w, "the action changed after this card was shown; review it again",
			"content_changed", http.StatusConflict)
		return c, false
	}
	return c, true
}

func (h *approvalCardHandlers) approve(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.cardTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		CardHash string `json:"card_hash"`
	}
	if !decodeCardBody(w, r, &req) {
		return
	}
	c, ok := pendingCard(w, r, inst, id, req.CardHash, true)
	if !ok {
		return
	}
	user := UserFromContext(r.Context())
	body, refused := approveAndRunExpecting(r.Context(), store.NewActionStore(inst.Pool),
		inst.Executor, id, user.ID, c.SQL)
	if refused != nil {
		refused.writeCode(w, id)
		return
	}
	body["database"] = inst.Name
	jsonResponse(w, body)
}

func (h *approvalCardHandlers) reject(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.cardTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason   string `json:"reason"`
		CardHash string `json:"card_hash"`
	}
	if !decodeCardBody(w, r, &req) {
		return
	}
	if req.Reason = strings.TrimSpace(req.Reason); req.Reason == "" {
		sreErrorCode(w, "reason is required", "invalid_request", http.StatusBadRequest)
		return
	}
	if _, ok := pendingCard(w, r, inst, id, req.CardHash, false); !ok {
		return
	}
	user := UserFromContext(r.Context())
	if err := store.NewActionStore(inst.Pool).Reject(r.Context(), id, user.ID,
		req.Reason); err != nil {
		sreErrorCode(w, "the action is no longer awaiting approval", "not_pending",
			http.StatusConflict)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "queue_id": id, "database": inst.Name,
		"status": "rejected"})
}

func (h *approvalCardHandlers) snooze(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.cardTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Hours  json.Number `json:"hours"`
		Reason string      `json:"reason"`
	}
	if !decodeCardBody(w, r, &req) {
		return
	}
	hours, err := req.Hours.Int64()
	if req.Hours == "" {
		hours, err = defaultSnoozeHours, nil
	}
	if err != nil || hours < 1 || hours > maxSnoozeHours {
		sreErrorCode(w, "hours must be a whole number from 1 to 168", "invalid_request",
			http.StatusBadRequest)
		return
	}
	user := UserFromContext(r.Context())
	until, err := snoozeItem(r.Context(), store.NewActionStore(inst.Pool), id, user.ID,
		time.Duration(hours)*time.Hour, req.Reason)
	if err != nil {
		sreErrorCode(w, "the action is no longer awaiting approval", "not_pending",
			http.StatusConflict)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "queue_id": id, "database": inst.Name,
		"status": "snoozed", "snoozed_until": until})
}

// snoozeItem snoozes a pending item for d with a reason (a default one
// when none is given).
func snoozeItem(ctx context.Context, as *store.ActionStore, id, userID int,
	d time.Duration, reason string) (time.Time, error) {
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "snoozed for " + d.String()
	}
	until := time.Now().Add(d).UTC()
	return until, as.Snooze(ctx, id, userID, until, reason)
}

// writeCode answers a refused card approval with a stable error code.
func (r *approvalRefusal) writeCode(w http.ResponseWriter, queueID int) {
	switch {
	case r.approveErr != nil:
		slog.Warn("approval card refused", "queue_id", queueID, "error", r.approveErr)
		sreErrorCode(w, "the action is no longer awaiting approval, or it changed",
			"not_pending", http.StatusConflict)
	case r.status == http.StatusNotFound:
		sreErrorCode(w, "queue item not found", "not_found", http.StatusNotFound)
	default:
		sreErrorCode(w, r.msg, "not_eligible", r.status)
	}
}
