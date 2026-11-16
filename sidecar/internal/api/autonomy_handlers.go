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
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/rollout"
)

// Sage SRE M7 earned-autonomy routes (AI-SRE-SPEC §7.3, §9). Every
// signed-in role reads the ledger, its evidence and history. Operators
// review packets, flag harm, ask for an evaluation, reject proposals and
// downgrade: restricting autonomy is always allowed. Only an admin
// approves a promotion or re-approves a level a PostgreSQL major upgrade
// capped (the human trust step, recorded with the approver) or uploads
// benchmark evidence.

// AutonomyDeps wires the autonomy routes.
type AutonomyDeps struct {
	Ledgers  *earned.Registry
	GameDays *gameday.Registry
	// LocalBench runs the bench locally on a clone (roadmap 1.1).
	LocalBench *gameday.BenchRegistry
	Canary     *rollout.CanaryService
	// FastElevation is every trust-elevation setting below the spec (the
	// config is restart-bound, so it is fixed for the process).
	FastElevation []config.LoweredSetting
	// BenchVerifier verifies a signed bench upload's Sigstore bundle
	// (benches-09); without one a signed upload is refused (503).
	BenchVerifier BenchVerifier
}

const autonomyPath = "/api/v1/sre/autonomy"

func registerAutonomyRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	deps *AutonomyDeps) {
	if deps == nil || deps.Ledgers == nil {
		return
	}
	h := autonomyHandlers{deps: deps, mgr: mgr}
	route(mux, "GET "+autonomyPath, gateViewer, http.HandlerFunc(h.view))
	route(mux, "GET "+autonomyPath+"/history", gateViewer, http.HandlerFunc(h.history))
	route(mux, "GET "+autonomyPath+"/proposals", gateViewer, http.HandlerFunc(h.proposals))
	routeDB(mux, "POST "+autonomyPath+"/evaluate", gateOperator, queryDatabase,
		http.HandlerFunc(h.evaluate))
	route(mux, "POST "+autonomyPath+"/proposals/{id}/approve", gateAdmin,
		http.HandlerFunc(h.approve))
	routeDB(mux, "POST "+autonomyPath+"/proposals/{id}/reject", gateOperator, queryDatabase,
		http.HandlerFunc(h.reject))
	routeDB(mux, "POST "+autonomyPath+"/downgrade", gateOperator, queryDatabase,
		http.HandlerFunc(h.downgrade))
	route(mux, "POST "+autonomyPath+"/revalidate", gateAdmin, http.HandlerFunc(h.revalidate))
	routeDB(mux, "POST "+autonomyPath+"/reviews", gateOperator, queryDatabase,
		http.HandlerFunc(h.review))
	routeDB(mux, "POST "+autonomyPath+"/outcomes", gateOperator, queryDatabase,
		http.HandlerFunc(h.outcome))
	route(mux, "POST "+autonomyPath+"/bench-results", gateAdmin, http.HandlerFunc(h.bench))
	route(mux, "GET "+autonomyPath+"/game-days", gateViewer, http.HandlerFunc(h.gameDays))
	route(mux, "POST "+autonomyPath+"/game-days", gateAdmin, http.HandlerFunc(h.startGameDay))
	route(mux, "GET "+autonomyPath+"/bench-runs", gateViewer, http.HandlerFunc(h.benchRuns))
	route(mux, "POST "+autonomyPath+"/bench-runs", gateAdmin, http.HandlerFunc(h.startBenchRun))
	route(mux, "GET "+autonomyPath+"/rollouts", gateViewer, http.HandlerFunc(h.rollouts))
	route(mux, "GET "+autonomyPath+"/rollouts/{id}", gateViewer, http.HandlerFunc(h.rollout))
	route(mux, "POST "+autonomyPath+"/rollouts", gateAdmin, http.HandlerFunc(h.startRollout))
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
		errors.Is(err, rollout.ErrInvalidCanary), errors.Is(err, gameday.ErrUnknownFamily):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
	case errors.Is(err, earned.ErrNotFound):
		sreErrorCode(w, err.Error(), "not_found", http.StatusNotFound)
	case errors.Is(err, earned.ErrHumanApprovalRequired):
		sreErrorCode(w, err.Error(), "human_approval_required", http.StatusForbidden)
	case errors.Is(err, earned.ErrNotADowngrade):
		sreErrorCode(w, err.Error(), "not_a_downgrade", http.StatusConflict)
	case errors.Is(err, earned.ErrNotCapped):
		sreErrorCode(w, err.Error(), "not_capped", http.StatusConflict)
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
	waiting, _ := e.Service.Proposal(r.Context(), r.PathValue("id"))
	st, err := e.Service.Approve(r.Context(), r.PathValue("id"),
		autonomyActor(UserFromContext(r.Context())), body.Note)
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	recordDecision(r, inbox.KindAutonomyPromotion, waiting.ProposedAt)
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
	recordDecision(r, inbox.KindAutonomyPromotion, p.ProposedAt)
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
