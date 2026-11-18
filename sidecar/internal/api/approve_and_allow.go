package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/store"
)

// Approve once, allow always (ux-07a): POST
// /api/v1/approvals/{id}/approve-and-allow approves the card's action
// through the ordinary approval path, then approves its class's L3
// promotion. The card must still offer allow-always and the caller must
// be a person; otherwise neither step runs. If the action approval
// fails, no promotion is made; if the promotion fails, the action
// approval stands and the response says why.

// alwaysAllower grants a class L3 for a person (*earned.Service is one).
type alwaysAllower interface {
	AllowAlways(ctx context.Context, f earned.Family, c earned.ActionClass, approver,
		note string) (earned.State, error)
}

// allowAlwaysRequest is the route's body; family and class, when given,
// must name the card's own class.
type allowAlwaysRequest struct {
	CardHash string `json:"card_hash"`
	Family   string `json:"family"`
	Class    string `json:"class"`
	Note     string `json:"note"`
}

// allowerFor is the database's ledger for allow-always.
func (h *approvalCardHandlers) allowerFor(database string) (alwaysAllower, bool) {
	if h.allower != nil {
		return h.allower(database)
	}
	if h.ledgers == nil {
		return nil, false
	}
	e, ok := h.ledgers.Lookup(database)
	if !ok || e.Service == nil {
		return nil, false
	}
	return e.Service, true
}

// cardApprover is the audit identity of the caller: a person signed in,
// or "mcp:<actor>" for a token caller (an agent never grants autonomy).
func cardApprover(r *http.Request) string {
	if p, ok := mcp.PrincipalFromContext(r.Context()); ok {
		return "mcp:" + p.Actor
	}
	return autonomyActor(UserFromContext(r.Context()))
}

func (h *approvalCardHandlers) approveAndAllow(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.cardTarget(w, r)
	if !ok {
		return
	}
	var req allowAlwaysRequest
	if !decodeCardBody(w, r, &req) || !validAllowRequest(w, req) {
		return
	}
	approver := cardApprover(r)
	if err := earned.CheckHumanApprover(approver); err != nil {
		sreErrorCode(w, err.Error(), "human_approval_required", http.StatusForbidden)
		return
	}
	allower, ok := h.allowerFor(inst.Name)
	if !ok {
		sreErrorCode(w, "no ledger for database "+inst.Name+": autonomy cannot be raised "+
			"here", "no_ledger", http.StatusConflict)
		return
	}
	c, ok := h.pendingCard(w, r, inst, id, req.CardHash, true)
	if !ok || !cardAllowsAlways(w, c, req) {
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
	h.grantAlways(r.Context(), allower, inst, c, approver, req.Note, body)
	jsonResponse(w, body)
}

// validAllowRequest refuses a body no grant could take.
func validAllowRequest(w http.ResponseWriter, req allowAlwaysRequest) bool {
	if len(req.Note) > 2000 {
		sreErrorCode(w, "note longer than 2000 characters", "invalid_request",
			http.StatusBadRequest)
		return false
	}
	return true
}

// cardAllowsAlways checks the card still offers allow-always for the
// class the body names.
func cardAllowsAlways(w http.ResponseWriter, c approvalcard.Card,
	req allowAlwaysRequest) bool {
	t := c.Trust
	switch {
	case t == nil:
		sreErrorCode(w, "no ledger judges this card's class: autonomy cannot be raised",
			"no_ledger", http.StatusConflict)
		return false
	case t.Unavailable != "":
		sreErrorCode(w, "the trust ledger is unavailable: "+t.Unavailable,
			"metadata_unavailable", http.StatusServiceUnavailable)
		return false
	case (req.Class != "" && req.Class != t.Class) ||
		(req.Family != "" && req.Family != t.Family):
		sreErrorCode(w, fmt.Sprintf("this card's class is %s/%s, not %s/%s", t.Family,
			t.Class, orAny(req.Family), orAny(req.Class)), "invalid_request",
			http.StatusBadRequest)
		return false
	case !t.AllowAlways:
		sreErrorCode(w, "this card no longer offers allow-always: "+t.AllowAlwaysWhy,
			"not_allowed_always", http.StatusConflict)
		return false
	}
	return true
}

func orAny(s string) string {
	if strings.TrimSpace(s) == "" {
		return "*"
	}
	return s
}

// grantAlways approves the class's L3 after the action approval and
// records the outcome on body; a failure leaves the approval standing.
func (h *approvalCardHandlers) grantAlways(ctx context.Context, allower alwaysAllower,
	inst *fleet.DatabaseInstance, c approvalcard.Card, approver, extra string,
	body map[string]any) {
	note := fmt.Sprintf("allow always from card %d", c.QueueID)
	if extra = strings.TrimSpace(extra); extra != "" {
		note = truncateNote(note + ": " + extra)
	}
	f, cl := earned.Family(c.Trust.Family), earned.ActionClass(c.Trust.Class)
	if ok, _ := body["ok"].(bool); !ok {
		body["allow_always"] = false
		body["autonomy_error"] = "autonomy unchanged: the action did not run"
		return
	}
	st, err := allower.AllowAlways(ctx, f, cl, approver, note)
	if err != nil {
		slog.Warn("allow always: promotion refused after the approval",
			"database", inst.Name, "queue_id", c.QueueID, "class", cl, "error", err)
		body["allow_always"] = false
		body["autonomy_error"] = "approved; autonomy unchanged: " + err.Error()
		body["autonomy_code"] = allowErrorCode(err)
		return
	}
	body["allow_always"] = true
	body["autonomy_level"] = st.Level.String()
	body["autonomy"] = st
}

func truncateNote(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}

// allowErrorCode is the stable code of a refused promotion.
func allowErrorCode(err error) string {
	switch {
	case errors.Is(err, earned.ErrEvidenceNotMet):
		return "evidence_not_met"
	case errors.Is(err, earned.ErrNotPending), errors.Is(err, earned.ErrConflict),
		errors.Is(err, earned.ErrProposalExpired):
		return "conflict"
	case errors.Is(err, earned.ErrHumanApprovalRequired):
		return "human_approval_required"
	}
	return "ledger_error"
}
