package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/gameday"
	"github.com/pg-sage/sidecar/internal/rollout"
)

// Sage SRE M7 earned-autonomy routes (AI-SRE-SPEC §7.3, §9). Every
// signed-in role reads the ledger, its evidence and history. Operators
// review packets, flag harm, ask for an evaluation, reject proposals and
// downgrade: restricting autonomy is always allowed. Only an admin
// approves a promotion (the human trust step, recorded with the
// approver) or uploads benchmark evidence.

// AutonomyDeps wires the autonomy routes.
type AutonomyDeps struct {
	Ledgers  *earned.Registry
	GameDays *gameday.Registry
	Canary   *rollout.CanaryService
	// FastElevation is every trust-elevation setting below the spec (the
	// config is restart-bound, so it is fixed for the process).
	FastElevation []config.LoweredSetting
}

const autonomyPath = "/api/v1/sre/autonomy"

func registerAutonomyRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	deps *AutonomyDeps) {
	if deps == nil || deps.Ledgers == nil {
		return
	}
	viewer := RequireRole("admin", "operator", "viewer")
	operator := RequireRole("admin", "operator")
	admin := RequireRole("admin")
	h := autonomyHandlers{deps: deps, mgr: mgr}
	mux.Handle("GET "+autonomyPath, viewer(http.HandlerFunc(h.view)))
	mux.Handle("GET "+autonomyPath+"/history", viewer(http.HandlerFunc(h.history)))
	mux.Handle("GET "+autonomyPath+"/proposals", viewer(http.HandlerFunc(h.proposals)))
	mux.Handle("POST "+autonomyPath+"/evaluate", operator(http.HandlerFunc(h.evaluate)))
	mux.Handle("POST "+autonomyPath+"/proposals/{id}/approve",
		admin(http.HandlerFunc(h.approve)))
	mux.Handle("POST "+autonomyPath+"/proposals/{id}/reject",
		operator(http.HandlerFunc(h.reject)))
	mux.Handle("POST "+autonomyPath+"/downgrade", operator(http.HandlerFunc(h.downgrade)))
	mux.Handle("POST "+autonomyPath+"/reviews", operator(http.HandlerFunc(h.review)))
	mux.Handle("POST "+autonomyPath+"/outcomes", operator(http.HandlerFunc(h.outcome)))
	mux.Handle("POST "+autonomyPath+"/bench-results", admin(http.HandlerFunc(h.bench)))
	mux.Handle("GET "+autonomyPath+"/game-days", viewer(http.HandlerFunc(h.gameDays)))
	mux.Handle("POST "+autonomyPath+"/game-days", admin(http.HandlerFunc(h.startGameDay)))
	mux.Handle("GET "+autonomyPath+"/rollouts", viewer(http.HandlerFunc(h.rollouts)))
	mux.Handle("GET "+autonomyPath+"/rollouts/{id}", viewer(http.HandlerFunc(h.rollout)))
	mux.Handle("POST "+autonomyPath+"/rollouts", admin(http.HandlerFunc(h.startRollout)))
}

type autonomyHandlers struct {
	deps *AutonomyDeps
	mgr  *fleet.DatabaseManager
}

// autonomyActor is the audit identity of the signed-in user.
func autonomyActor(u *auth.User) string {
	if u == nil {
		return ""
	}
	return fmt.Sprintf("user:%d:%s", u.ID, u.Email)
}

// ledger resolves the ledger of a database (the only one when unnamed).
func (h autonomyHandlers) ledger(w http.ResponseWriter, name string) (earned.RegistryEntry,
	string, bool) {
	if name == "" {
		dbs := h.deps.Ledgers.Databases()
		if len(dbs) != 1 {
			sreErrorCode(w, "database is required", "invalid_request", http.StatusBadRequest)
			return earned.RegistryEntry{}, "", false
		}
		name = dbs[0]
	}
	e, ok := h.deps.Ledgers.Lookup(name)
	if !ok {
		sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
		return earned.RegistryEntry{}, "", false
	}
	return e, name, true
}

// autonomyError writes a canonical error code for a ledger error.
func autonomyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, earned.ErrEvidenceNotMet):
		sreErrorCode(w, err.Error(), "evidence_not_met", http.StatusConflict)
	case errors.Is(err, earned.ErrInvalidRequest), errors.Is(err, earned.ErrInvalidReport),
		errors.Is(err, rollout.ErrInvalidCanary):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
	case errors.Is(err, earned.ErrNotFound):
		sreErrorCode(w, err.Error(), "not_found", http.StatusNotFound)
	case errors.Is(err, earned.ErrHumanApprovalRequired):
		sreErrorCode(w, err.Error(), "human_approval_required", http.StatusForbidden)
	case errors.Is(err, earned.ErrNotADowngrade):
		sreErrorCode(w, err.Error(), "not_a_downgrade", http.StatusConflict)
	case errors.Is(err, earned.ErrNotPending), errors.Is(err, earned.ErrProposalExpired),
		errors.Is(err, earned.ErrConflict):
		sreErrorCode(w, err.Error(), "conflict", http.StatusConflict)
	case errors.Is(err, rollout.ErrSourceNotVerified):
		sreErrorCode(w, err.Error(), "source_not_verified", http.StatusConflict)
	case errors.Is(err, rollout.ErrCanaryRunning), errors.Is(err, gameday.ErrRunning):
		sreErrorCode(w, err.Error(), "running", http.StatusConflict)
	case errors.Is(err, earned.ErrUnavailable):
		sreErrorCode(w, "autonomy ledger unavailable", "metadata_unavailable",
			http.StatusServiceUnavailable)
	default:
		internalError(w, r, "sre autonomy", err)
	}
}

// decodeAutonomyBody decodes an optional JSON body strictly; empty is allowed.
func decodeAutonomyBody(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		sreErrorCode(w, "request body unreadable", "invalid_request", http.StatusBadRequest)
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		sreErrorCode(w, "invalid JSON: "+err.Error(), "invalid_request", http.StatusBadRequest)
		return false
	}
	return true
}

func (h autonomyHandlers) view(w http.ResponseWriter, r *http.Request) {
	e, name, ok := h.ledger(w, r.URL.Query().Get("database"))
	if !ok {
		return
	}
	v, err := e.Service.View(r.Context())
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	if e.Limiter != nil {
		e.Limiter.Annotate(r.Context(), &v)
	}
	jsonResponse(w, map[string]any{"database": name, "enforced": h.deps.Ledgers.Enforced(),
		"databases": h.deps.Ledgers.Databases(), "view": v,
		"fast_elevation": fastElevationView(h.deps.FastElevation)})
}

func (h autonomyHandlers) history(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.ledger(w, r.URL.Query().Get("database"))
	if !ok {
		return
	}
	q := r.URL.Query()
	f := earned.EventFilter{Family: earned.Family(q.Get("family")),
		Class: earned.ActionClass(q.Get("class"))}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			sreErrorCode(w, "limit must be an integer", "invalid_request", http.StatusBadRequest)
			return
		}
		f.Limit = n
	}
	items, err := e.Service.History(r.Context(), f)
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"items": items})
}

func (h autonomyHandlers) proposals(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.ledger(w, r.URL.Query().Get("database"))
	if !ok {
		return
	}
	items, err := e.Service.PendingProposals(r.Context())
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"items": items})
}

func (h autonomyHandlers) evaluate(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.ledger(w, r.URL.Query().Get("database"))
	if !ok {
		return
	}
	evaluation, err := e.Service.Evaluate(r.Context())
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, evaluation)
}

func (h autonomyHandlers) approve(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.ledger(w, r.URL.Query().Get("database"))
	var body struct {
		Note string `json:"note"`
	}
	if !ok || !decodeAutonomyBody(w, r, &body) {
		return
	}
	st, err := e.Service.Approve(r.Context(), r.PathValue("id"),
		autonomyActor(UserFromContext(r.Context())), body.Note)
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"state": st})
}

func (h autonomyHandlers) reject(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.ledger(w, r.URL.Query().Get("database"))
	var body struct {
		Note string `json:"note"`
	}
	if !ok || !decodeAutonomyBody(w, r, &body) {
		return
	}
	p, err := e.Service.Reject(r.Context(), r.PathValue("id"),
		autonomyActor(UserFromContext(r.Context())), body.Note)
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"proposal": p})
}

func (h autonomyHandlers) downgrade(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.ledger(w, r.URL.Query().Get("database"))
	var body struct {
		Family string       `json:"family"`
		Class  string       `json:"class"`
		Level  earned.Level `json:"level"`
		Reason string       `json:"reason"`
	}
	if !ok || !decodeAutonomyBody(w, r, &body) {
		return
	}
	states, err := e.Service.Downgrade(r.Context(), earned.DowngradeRequest{
		Family: earned.Family(body.Family), Class: earned.ActionClass(body.Class),
		To: body.Level, Reason: body.Reason, Actor: autonomyActor(UserFromContext(r.Context()))})
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"states": states})
}

// fastElevationView says whether elevation settings are below the spec
// and lists them; the list is never null.
func fastElevationView(lowered []config.LoweredSetting) map[string]any {
	if lowered == nil {
		lowered = []config.LoweredSetting{}
	}
	return map[string]any{"active": len(lowered) > 0, "lowered": lowered}
}
