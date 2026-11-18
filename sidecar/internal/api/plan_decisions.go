package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// The plan decision handlers (plan-03a). Every decision names the plan
// hash the person was shown (cancel aside: stopping is always allowed); a
// changed plan is 409 content_changed, as for approval cards. Input
// errors are 400 with the store's code; nothing internal reaches the body.

const maxPlanBody = 16 << 10

// decodePlanBody reads a decision body; an empty body is invalid unless
// optional.
func decodePlanBody(w http.ResponseWriter, r *http.Request, out any, optional bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPlanBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(out)
	if err == nil || (optional && errors.Is(err, io.EOF)) {
		return true
	}
	sreErrorCode(w, "invalid JSON body", "invalid_request", http.StatusBadRequest)
	return false
}

// decisionVia is where the caller decides: a token caller (the CLI or a
// script) says cli or api (api by default); a signed-in caller ui or api
// (ui by default). Another channel is refused, so a token cannot claim a
// person clicked in the dashboard.
func decisionVia(w http.ResponseWriter, r *http.Request, asked string) (changeplan.Via, bool) {
	_, token := mcp.PrincipalFromContext(r.Context())
	allowed, fallback := []changeplan.Via{changeplan.ViaUI, changeplan.ViaAPI}, changeplan.ViaUI
	if token {
		allowed, fallback = []changeplan.Via{changeplan.ViaCLI, changeplan.ViaAPI},
			changeplan.ViaAPI
	}
	if asked == "" {
		return fallback, true
	}
	for _, v := range allowed {
		if string(v) == asked {
			return v, true
		}
	}
	sreErrorCode(w, "via "+strconv.Quote(asked)+" is not allowed for this caller",
		changeplan.CodeInvalidVia, http.StatusBadRequest)
	return "", false
}

func (h *planHandlers) approve(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.planTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		PlanHash string `json:"plan_hash"`
		Mode     string `json:"mode"`
		UpTo     int    `json:"up_to"`
		Via      string `json:"via"`
	}
	if !decodePlanBody(w, r, &req, false) {
		return
	}
	via, ok := decisionVia(w, r, req.Via)
	if !ok {
		return
	}
	d := changeplan.Decision{PlanHash: strings.TrimSpace(req.PlanHash),
		Mode: changeplan.Mode(req.Mode), UpTo: req.UpTo, Via: via}
	if err := d.Check(); err != nil {
		writePlanError(w, r, "approve plan", err)
		return
	}
	a, err := h.stores(inst).ApproveWith(r.Context(), id, d, UserFromContext(r.Context()).ID)
	if err != nil {
		writePlanError(w, r, "approve plan", err)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "database": inst.Name, "plan_id": id,
		"status": changeplan.StatusApproved, "approval": a})
}

func (h *planHandlers) approveStep(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.planTarget(w, r)
	if !ok {
		return
	}
	stepID, err := strconv.ParseInt(r.PathValue("step"), 10, 64)
	if err != nil || stepID <= 0 {
		sreErrorCode(w, "invalid step id", "invalid_request", http.StatusBadRequest)
		return
	}
	var req struct {
		PlanHash string `json:"plan_hash"`
		Via      string `json:"via"`
	}
	if !decodePlanBody(w, r, &req, false) {
		return
	}
	via, ok := decisionVia(w, r, req.Via)
	if !ok {
		return
	}
	if strings.TrimSpace(req.PlanHash) == "" {
		sreErrorCode(w, "plan_hash is required", changeplan.CodeHashRequired,
			http.StatusBadRequest)
		return
	}
	sa, err := h.stores(inst).ApproveStep(r.Context(), id, stepID,
		strings.TrimSpace(req.PlanHash), UserFromContext(r.Context()).ID, via)
	if err != nil {
		writePlanError(w, r, "approve plan step", err)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "database": inst.Name, "plan_id": id,
		"step_approval": sa})
}

func (h *planHandlers) reject(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.planTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if !decodePlanBody(w, r, &req, false) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		sreErrorCode(w, "reason is required", changeplan.CodeReasonRequired,
			http.StatusBadRequest)
		return
	}
	err := h.stores(inst).Reject(r.Context(), id, req.Reason, UserFromContext(r.Context()).ID)
	h.answerEnd(w, r, inst.Name, id, "reject plan", changeplan.StatusRejected, err)
}

func (h *planHandlers) resume(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.planTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		PlanHash string `json:"plan_hash"`
	}
	if !decodePlanBody(w, r, &req, false) {
		return
	}
	if strings.TrimSpace(req.PlanHash) == "" {
		sreErrorCode(w, "plan_hash is required", changeplan.CodeHashRequired,
			http.StatusBadRequest)
		return
	}
	err := h.stores(inst).Resume(r.Context(), id, strings.TrimSpace(req.PlanHash),
		UserFromContext(r.Context()).ID)
	h.answerEnd(w, r, inst.Name, id, "resume plan", changeplan.StatusRunning, err)
}

func (h *planHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.planTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if !decodePlanBody(w, r, &req, true) {
		return
	}
	err := h.stores(inst).Cancel(r.Context(), id, req.Reason, UserFromContext(r.Context()).ID)
	h.answerEnd(w, r, inst.Name, id, "cancel plan", changeplan.StatusCancelled, err)
}

// answerEnd answers a reject, resume or cancel: the plan's new status, or
// the refusal.
func (h *planHandlers) answerEnd(w http.ResponseWriter, r *http.Request, database string,
	id int64, op string, status changeplan.Status, err error) {
	if err != nil {
		writePlanError(w, r, op, err)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "database": database, "plan_id": id,
		"status": status})
}

// planRefusals maps the store's refusals to their status, code and the
// message the caller sees.
var planRefusals = []struct {
	err    error
	status int
	code   string
	msg    string
}{
	{changeplan.ErrNotFound, http.StatusNotFound, "not_found", "plan not found"},
	{changeplan.ErrStaleHash, http.StatusConflict, "content_changed",
		"the plan changed after it was shown; review it again"},
	{changeplan.ErrHashMismatch, http.StatusConflict, "content_changed",
		"the plan's rows no longer match its approval; review it again"},
	{changeplan.ErrNotApproved, http.StatusConflict, "not_approved",
		"the plan has no live approval"},
	{changeplan.ErrNotApprovable, http.StatusConflict, "not_pending",
		"the plan is not awaiting approval in its status"},
	{changeplan.ErrNotRejectable, http.StatusConflict, "not_pending",
		"only a proposed plan can be rejected"},
	{changeplan.ErrStepNotApprovable, http.StatusConflict, "step_not_pending",
		"the step has started or ended; only a waiting step can be approved"},
	{changeplan.ErrNotResumable, http.StatusConflict, "not_halted",
		"only a halted plan can be resumed"},
	{changeplan.ErrNotCancellable, http.StatusConflict, "not_cancellable",
		"only an approved or halted plan, or a running one with no step in flight, can " +
			"be cancelled; a step in flight ends first, or use the emergency stop"},
}

// writePlanError answers err: an input error with its code, a refusal
// with its status, anything else as an internal error (logged, not
// shown).
func writePlanError(w http.ResponseWriter, r *http.Request, op string, err error) {
	var de *changeplan.DecisionError
	var ce *changeplan.CapError
	var ip *changeplan.InvalidPlanError
	switch {
	case errors.As(err, &de):
		sreErrorCode(w, de.Detail, de.Code, http.StatusBadRequest)
		return
	case errors.As(err, &ce):
		sreErrorCode(w, ce.Error(), "over_cap", http.StatusConflict)
		return
	case errors.As(err, &ip):
		sreErrorCode(w, ip.Error(), "invalid_plan", http.StatusConflict)
		return
	}
	for _, ref := range planRefusals {
		if errors.Is(err, ref.err) {
			sreErrorCode(w, ref.msg, ref.code, ref.status)
			return
		}
	}
	internalError(w, r, op, err)
}
