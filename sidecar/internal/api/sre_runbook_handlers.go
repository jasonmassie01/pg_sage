package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Typed runbook and incident memory routes (AI-SRE-SPEC §7.1, §9). Viewers
// list and read runbooks, their run history and an investigation's
// similar past incidents. Operators write drafts (JSON or English compiled
// by the model), retire runbooks and record investigation outcomes. Only
// admins sign, and a signature names the version and the content hash the
// signer reviewed. Every route is scoped to the named database. Nothing
// here runs a runbook or executes a proposal.

const runbooksPath = "/api/v1/databases/{db}/runbooks"

// maxRunbookBody bounds a runbook request body (definition or playbook).
const maxRunbookBody = 64 << 10

func registerRunbookRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	viewerUp := RequireRole("admin", "operator", "viewer")
	operatorUp := RequireRole("admin", "operator")
	adminOnly := RequireRole("admin")
	one := runbooksPath + "/{id}"
	mux.Handle("GET "+runbooksPath, viewerUp(runbookListHandler(mgr)))
	mux.Handle("POST "+runbooksPath, operatorUp(runbookCreateHandler(mgr)))
	mux.Handle("POST "+runbooksPath+"/compile", operatorUp(runbookCompileHandler(mgr)))
	mux.Handle("GET "+one, viewerUp(runbookGetHandler(mgr)))
	mux.Handle("GET "+one+"/runs", viewerUp(runbookRunsHandler(mgr)))
	mux.Handle("POST "+one+"/versions", operatorUp(runbookReviseHandler(mgr)))
	mux.Handle("POST "+one+"/retire", operatorUp(runbookRetireHandler(mgr)))
	mux.Handle("POST "+one+"/sign", adminOnly(runbookSignHandler(mgr)))
	mux.Handle("GET "+sreInvestigationsPath+"/{id}/similar",
		viewerUp(similarIncidentsHandler(mgr)))
	mux.Handle("POST "+sreInvestigationsPath+"/{id}/outcome",
		operatorUp(outcomeHandler(mgr)))
}

// runbookError writes a canonical error code for runbook and memory errors.
func runbookError(w http.ResponseWriter, r *http.Request, err error) {
	var rej *runbook.Rejection
	var problems runbook.Problems
	switch {
	case errors.As(err, &rej):
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "runbook draft rejected", "code": "compile_rejected",
			"reason": rej.Reason, "detail": rej.Detail, "problems": rej.Problems})
	case errors.Is(err, sre.ErrRunbookNotFound):
		sreErrorCode(w, "runbook not found", "not_found", http.StatusNotFound)
	case errors.Is(err, sre.ErrModelUnavailable):
		sreErrorCode(w, err.Error(), "missing_capability", http.StatusServiceUnavailable)
	case errors.Is(err, sre.ErrVersionConflict):
		sreErrorCode(w, err.Error(), "version_conflict", http.StatusConflict)
	case errors.Is(err, sre.ErrHashMismatch):
		sreErrorCode(w, err.Error(), "hash_mismatch", http.StatusConflict)
	case errors.Is(err, sre.ErrAlreadySigned):
		sreErrorCode(w, err.Error(), "already_signed", http.StatusConflict)
	case errors.Is(err, sre.ErrRetired):
		sreErrorCode(w, err.Error(), "retired", http.StatusConflict)
	case errors.Is(err, sre.ErrInvalidRequest) && errors.As(err, &problems):
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error(),
			"code": "invalid_request", "problems": problems})
	default:
		sreErrorResponse(w, r, err)
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decodeBody reads a bounded JSON body strictly into target.
func decodeBody(r *http.Request, target any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRunbookBody+1))
	if err != nil {
		return fmt.Errorf("%w: reading the body: %v", sre.ErrInvalidRequest, err)
	}
	if len(raw) > maxRunbookBody {
		return fmt.Errorf("%w: body over %d bytes", sre.ErrInvalidRequest, maxRunbookBody)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("%w: %v", sre.ErrInvalidRequest, err)
	}
	return nil
}

// definitionOf decodes a request's definition strictly.
func definitionOf(raw json.RawMessage) (runbook.Definition, error) {
	if len(raw) == 0 {
		return runbook.Definition{}, fmt.Errorf("%w: definition is required",
			sre.ErrInvalidRequest)
	}
	d, err := runbook.Decode(raw)
	if err != nil {
		return d, fmt.Errorf("%w: %v", sre.ErrInvalidRequest, err)
	}
	return d, nil
}

func actorOf(r *http.Request) string {
	return fmt.Sprintf("user:%d", UserFromContext(r.Context()).ID)
}

// runbookID parses {id}; a malformed id is an invalid request.
func runbookID(r *http.Request) (sre.UUID, error) {
	return sre.ParseUUID(r.PathValue("id"))
}

func runbookListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		items, err := svc.Runbooks(r.Context())
		if err != nil {
			runbookError(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"database": svc.Name(), "items": items})
	}
}

func runbookGetHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		id, err := runbookID(r)
		if err == nil {
			var rb sre.Runbook
			if rb, err = svc.Runbook(r.Context(), id); err == nil {
				jsonResponse(w, rb)
				return
			}
		}
		runbookError(w, r, err)
	}
}

func runbookRunsHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		id, err := runbookID(r)
		if err == nil {
			var runs []sre.RunbookRun
			if runs, err = svc.RunbookRuns(r.Context(), id); err == nil {
				jsonResponse(w, map[string]any{"items": runs})
				return
			}
		}
		runbookError(w, r, err)
	}
}
