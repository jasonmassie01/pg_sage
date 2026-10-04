package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Contested investigations become replay cases (roadmap 2.4):
// GET /api/v1/databases/{db}/investigations/{id}/replay-case exports an
// investigation an operator refuted (or confirmed with another actual
// root) as a redacted PGIncidentBench replay case. Identifiers are keyed
// hashes unless ?keep_identifiers=true (the operator's opt-in); secrets
// and PII-like literals are removed either way. Operators and admins
// only, like the investigation export.

func registerReplayCaseRoute(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	mux.Handle("GET "+sreInvestigationsPath+"/{id}/replay-case",
		RequireRole("admin", "operator")(investigationReplayCaseHandler(mgr)))
}

func investigationReplayCaseHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keep, ok := keepIdentifiers(r.URL.Query().Get("keep_identifiers"))
		if !ok {
			sreErrorCode(w, "keep_identifiers must be true or false", "invalid_request",
				http.StatusBadRequest)
			return
		}
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		exp, err := svc.ExportReplayCase(r.Context(), sre.UUID(r.PathValue("id")),
			sre.ReplayExportOptions{KeepIdentifiers: keep})
		switch {
		case errors.Is(err, sre.ErrNotContested):
			sreErrorCode(w, err.Error(), "not_contested", http.StatusConflict)
		case errors.Is(err, sre.ErrNotExportable):
			sreErrorCode(w, err.Error(), "not_exportable", http.StatusUnprocessableEntity)
		case err != nil:
			sreErrorResponse(w, r, err)
		default:
			w.Header().Set("Content-Disposition", fmt.Sprintf(
				`attachment; filename="%s.json"`, exp.Case.ID))
			jsonResponse(w, exp)
		}
	}
}

func keepIdentifiers(v string) (bool, bool) {
	switch v {
	case "", "false", "0":
		return false, true
	case "true", "1":
		return true, true
	}
	return false, false
}
