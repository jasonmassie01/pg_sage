package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

func registerSREMaintenanceRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	base := sreInvestigationsPath + "/{id}/maintenance"
	mux.Handle("GET "+base, RequireRole("admin", "operator", "viewer")(maintenanceListHandler(mgr)))
	mux.Handle("POST "+base, RequireRole("admin", "operator")(maintenanceProposeHandler(mgr)))
	mux.Handle("POST "+base+"/{pid}/request", RequireRole("admin", "operator")(maintenanceMutationHandler(mgr, false)))
	mux.Handle("POST "+base+"/{pid}/rollback", RequireRole("admin")(maintenanceMutationHandler(mgr, true)))
}

func maintenanceService(w http.ResponseWriter, mgr *fleet.DatabaseManager, database string) (*sreaction.MaintenanceService, bool) {
	a, ok := actionService(w, mgr, database)
	if !ok {
		return nil, false
	}
	if m := a.MaintenanceService(); m != nil {
		return m, true
	}
	sreErrorCode(w, "maintenance unavailable for this database", "metadata_unavailable", http.StatusServiceUnavailable)
	return nil, false
}

func maintenanceListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := maintenanceService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		if _, err := m.Service.Detail(r.Context(), sre.UUID(r.PathValue("id"))); err != nil {
			sreActionError(w, r, err)
			return
		}
		items, err := m.ForInvestigation(r.Context(), sre.UUID(r.PathValue("id")))
		if err != nil {
			sreActionError(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"database": m.Service.Name(), "items": items})
	}
}

func readMaintenanceBody(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxSignedBody+1))
	if err != nil || len(raw) > maxSignedBody {
		sreErrorCode(w, "body too large or unreadable", "invalid_request", http.StatusRequestEntityTooLarge)
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(target); err != nil || dec.Decode(new(any)) != io.EOF {
		sreErrorCode(w, "invalid maintenance request object", "invalid_request", http.StatusBadRequest)
		return false
	}
	return true
}

func maintenanceProposeHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := maintenanceService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		var req sreaction.StatisticsRequest
		if !readMaintenanceBody(w, r, &req) {
			return
		}
		p, err := m.ProposeStatistics(r.Context(), sre.UUID(r.PathValue("id")), req, proposalActor(r))
		if err != nil {
			sreActionError(w, r, err)
			return
		}
		jsonResponse(w, p)
	}
}

func maintenanceMutationHandler(mgr *fleet.DatabaseManager, rollback bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := maintenanceService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		id, err := sre.ParseUUID(r.PathValue("pid"))
		var p sreaction.MaintenanceProposal
		if err == nil {
			p, err = m.Get(r.Context(), id)
		}
		if err == nil && p.InvestigationID != sre.UUID(r.PathValue("id")) {
			err = sreaction.ErrProposalNotFound
		}
		if err == nil {
			if rollback {
				var req struct {
					Reason string `json:"reason"`
				}
				if !readMaintenanceBody(w, r, &req) {
					return
				}
				p, err = m.Rollback(r.Context(), id, req.Reason, UserFromContext(r.Context()).ID)
			} else {
				p, err = m.RequestExecution(r.Context(), id, proposalActor(r))
			}
		}
		if err != nil {
			sreActionError(w, r, err)
			return
		}
		jsonResponse(w, p)
	}
}
