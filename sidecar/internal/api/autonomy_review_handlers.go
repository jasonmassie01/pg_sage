package api

import (
	"errors"
	"net/http"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/earned/packetreview"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Investigation reviews (2026-10-02 roadmap Phase 1.1). An operator's
// review of a finished investigation writes both records that describe
// it: the shadow review behind earned autonomy and the investigation
// outcome in incident memory (packetreview.Record). The investigation
// outcome route records the matching review too, so the two never
// disagree. The family comes from the investigation, never the caller.

// autonomyRegistry is the ledgers of the autonomy routes, nil without.
func autonomyRegistry(deps *AutonomyDeps) *earned.Registry {
	if deps == nil {
		return nil
	}
	return deps.Ledgers
}

// ledgerOf resolves a database's ledger, if it has one.
func ledgerOf(ledgers *earned.Registry, database string) (earned.RegistryEntry, bool) {
	if ledgers == nil {
		return earned.RegistryEntry{}, false
	}
	return ledgers.Lookup(database)
}

// investigationsOf is the investigation service of a fleet database, or
// nil (packetreview refuses it as unavailable).
func investigationsOf(mgr *fleet.DatabaseManager, database string) packetreview.Investigations {
	if mgr == nil {
		return nil
	}
	if inst := mgr.GetInstance(database); inst != nil && inst.Investigations != nil {
		return inst.Investigations
	}
	return nil
}

// review records an operator's verdict on a finished investigation.
func (h autonomyHandlers) review(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Database        string `json:"database"`
		InvestigationID string `json:"investigation_id"`
		Verdict         string `json:"verdict"`
		Note            string `json:"note"`
		ActualRootCause string `json:"actual_root_cause"`
	}
	if !decodeAutonomyBody(w, r, &body) {
		return
	}
	e, name, ok := h.ledger(w, body.Database)
	if !ok {
		return
	}
	res, err := packetreview.Record(r.Context(), e.Service, investigationsOf(h.mgr, name),
		packetreview.Request{InvestigationID: body.InvestigationID, Verdict: body.Verdict,
			Note: body.Note, ActualRootCause: body.ActualRootCause,
			Actor: autonomyActor(UserFromContext(r.Context()))})
	if err != nil {
		reviewError(w, r, err)
		return
	}
	jsonResponse(w, struct {
		OK bool `json:"ok"`
		packetreview.Result
	}{true, res})
}

// outcomeWithReview records an investigation outcome (confirmed or
// refuted) as a review: the outcome and the matching shadow review.
func outcomeWithReview(w http.ResponseWriter, r *http.Request, ledger *earned.Service,
	svc *sre.Service, verdict, actualNode string) {
	mapped := map[string]string{string(sre.OutcomeConfirmed): earned.VerdictAccepted,
		string(sre.OutcomeRefuted): earned.VerdictRejected}[verdict]
	if mapped == "" {
		sreErrorCode(w, "verdict must be confirmed or refuted", "invalid_request",
			http.StatusBadRequest)
		return
	}
	res, err := packetreview.Record(r.Context(), ledger, svc, packetreview.Request{
		InvestigationID: r.PathValue("id"), Verdict: mapped, ActualRootCause: actualNode,
		Actor: actorOf(r), RequireOutcome: true})
	if err != nil {
		reviewError(w, r, err)
		return
	}
	jsonResponse(w, res.Outcome)
}

// reviewError writes the canonical error of a review.
func reviewError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, packetreview.ErrNotFinished):
		sreErrorCode(w, err.Error(), "not_concluded", http.StatusConflict)
	case errors.Is(err, packetreview.ErrUnavailable):
		sreErrorCode(w, "investigations unavailable for this database", "not_found",
			http.StatusNotFound)
	case errors.Is(err, earned.ErrInvalidRequest), errors.Is(err, earned.ErrUnavailable):
		autonomyError(w, r, err)
	default:
		sreErrorResponse(w, r, err)
	}
}
