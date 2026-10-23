package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/agentguard/grants"
	"github.com/pg-sage/sidecar/internal/executor"
)

// Agent grants (spec §6.6, §8.3; G1: every grant is L2, operator-approved):
//
//	GET  /api/v1/agents/{id}/grants?database&state&limit&cursor
//	POST /api/v1/agents/{id}/grants                     {database, capability,
//	     objects:[{object, columns?}], duration_minutes, reason} → 201
//	POST /api/v1/agents/{id}/grants/{grant}/revoke      {database}
//	GET  /api/v1/agents/{id}/grant-requests?database&status&limit&cursor
//	POST /api/v1/agents/{id}/grant-requests/{request}/approve  {database}
//	POST /api/v1/agents/{id}/grant-requests/{request}/deny     {database}
//
// All are operator or admin. A refusal is 409 {verdict: blocked,
// reason_code, error, fix}; without a control database they answer 503.

// AgentGrantService is the grant API (*grants.Service).
type AgentGrantService interface {
	Grants(ctx context.Context, database string, f grants.Filter) (grants.Page, error)
	GrantNow(ctx context.Context, principalID string, in grants.CapabilityRequest,
		userID int) (grants.GrantResult, error)
	RevokeNow(ctx context.Context, database, principalID string, grantID int64,
		userID int) (grants.RevokeResult, error)
	Requests(ctx context.Context, database string,
		f grants.RequestFilter) (grants.RequestPage, error)
	Approve(ctx context.Context, database, principalID string, requestID int64,
		userID int) (grants.GrantResult, error)
	Deny(ctx context.Context, database, principalID string, requestID int64,
		userID int) (grants.Request, error)
}

const maxAgentGrantBody = 64 << 10

func registerAgentGrantRoutes(mux *http.ServeMux, svc AgentGrantService) {
	op := RequireRole("admin", "operator")
	h := agentGrantHandlers{svc: svc}
	const base = "/api/v1/agents/{id}"
	mux.Handle("GET "+base+"/grants", op(http.HandlerFunc(h.list)))
	mux.Handle("POST "+base+"/grants", op(http.HandlerFunc(h.grant)))
	mux.Handle("POST "+base+"/grants/{grant}/revoke", op(http.HandlerFunc(h.revoke)))
	mux.Handle("GET "+base+"/grant-requests", op(http.HandlerFunc(h.requests)))
	mux.Handle("POST "+base+"/grant-requests/{request}/approve",
		op(http.HandlerFunc(h.approve)))
	mux.Handle("POST "+base+"/grant-requests/{request}/deny", op(http.HandlerFunc(h.deny)))
}

type agentGrantHandlers struct{ svc AgentGrantService }

func unprocessable(w http.ResponseWriter, msg string) {
	sreErrorCode(w, msg, "invalid_arguments", http.StatusUnprocessableEntity)
}

// principal checks the service and the {id} path value.
func (h agentGrantHandlers) principal(w http.ResponseWriter, r *http.Request) (string,
	bool) {
	if h.svc == nil {
		sreErrorCode(w, "agent grants need mode: meta or agents.control_database",
			"unavailable", http.StatusServiceUnavailable)
		return "", false
	}
	id := r.PathValue("id")
	if !agentguard.ValidID(id) {
		unprocessable(w, "invalid agent id")
		return "", false
	}
	return id, true
}

func validDatabase(name string) bool {
	return name != "" && name != "all" && validateDatabaseParam(name) == nil
}

// pageQuery reads ?database&limit&cursor (limit defaults to 50).
func pageQuery(w http.ResponseWriter, r *http.Request) (string, int, string, bool) {
	q := r.URL.Query()
	db, limit := q.Get("database"), 50
	if !validDatabase(db) {
		unprocessable(w, "database is required")
		return "", 0, "", false
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 200 {
			unprocessable(w, "limit must be 1-200")
			return "", 0, "", false
		}
		limit = n
	}
	return db, limit, q.Get("cursor"), true
}

func (h agentGrantHandlers) list(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	db, limit, cursor, ok := pageQuery(w, r)
	if !ok {
		return
	}
	page, err := h.svc.Grants(r.Context(), db, grants.Filter{PrincipalID: pid,
		State: r.URL.Query().Get("state"), Limit: limit, Cursor: cursor})
	writeGrantResult(w, r, page, err, http.StatusOK)
}

func (h agentGrantHandlers) requests(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	db, limit, cursor, ok := pageQuery(w, r)
	if !ok {
		return
	}
	page, err := h.svc.Requests(r.Context(), db, grants.RequestFilter{PrincipalID: pid,
		Status: r.URL.Query().Get("status"), Limit: limit, Cursor: cursor})
	writeGrantResult(w, r, page, err, http.StatusOK)
}

func decodeGrantBody(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentGrantBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		unprocessable(w, "invalid request body")
		return false
	}
	return true
}

func (h agentGrantHandlers) grant(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	var in grants.CapabilityRequest
	if !decodeGrantBody(w, r, &in) {
		return
	}
	if !validDatabase(in.Database) {
		unprocessable(w, "database is required")
		return
	}
	res, err := h.svc.GrantNow(r.Context(), pid, in, userID(r))
	writeGrantResult(w, r, res, err, http.StatusCreated)
}

// idAndDatabase reads a positive path id and the {database} body.
func idAndDatabase(w http.ResponseWriter, r *http.Request, key string) (int64, string,
	bool) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		unprocessable(w, "invalid "+key+" id")
		return 0, "", false
	}
	var body struct {
		Database string `json:"database"`
	}
	if !decodeGrantBody(w, r, &body) {
		return 0, "", false
	}
	if !validDatabase(body.Database) {
		unprocessable(w, "database is required")
		return 0, "", false
	}
	return id, body.Database, true
}

func (h agentGrantHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, db, ok := idAndDatabase(w, r, "grant")
	if !ok {
		return
	}
	res, err := h.svc.RevokeNow(r.Context(), db, pid, id, userID(r))
	writeGrantResult(w, r, res, err, http.StatusOK)
}

func (h agentGrantHandlers) approve(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, db, ok := idAndDatabase(w, r, "request")
	if !ok {
		return
	}
	res, err := h.svc.Approve(r.Context(), db, pid, id, userID(r))
	writeGrantResult(w, r, res, err, http.StatusOK)
}

func (h agentGrantHandlers) deny(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, db, ok := idAndDatabase(w, r, "request")
	if !ok {
		return
	}
	res, err := h.svc.Deny(r.Context(), db, pid, id, userID(r))
	writeGrantResult(w, r, res, err, http.StatusOK)
}

func userID(r *http.Request) int {
	if u := UserFromContext(r.Context()); u != nil {
		return u.ID
	}
	return 0
}

func writeGrantResult(w http.ResponseWriter, r *http.Request, v any, err error,
	status int) {
	if err != nil {
		writeGrantError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func blockedBody(w http.ResponseWriter, reason, detail, fix string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]string{"verdict": "blocked",
		"reason_code": reason, "error": detail, "fix": fix})
}

func writeGrantError(w http.ResponseWriter, r *http.Request, err error) {
	var withheld *executor.WithheldError
	if d, ok := agentguard.IsDenied(err); ok {
		blockedBody(w, string(d.Reason), d.Detail, d.Fix)
		return
	}
	switch {
	case errors.As(err, &withheld):
		blockedBody(w, withheld.Decision.BlockedReason, err.Error(), "")
	case errors.Is(err, agentguard.ErrInvalid):
		unprocessable(w, err.Error())
	case errors.Is(err, agentguard.ErrNotFound), errors.Is(err, envbind.ErrUnknownDatabase):
		sreErrorCode(w, err.Error(), "not_found", http.StatusNotFound)
	case errors.Is(err, agentguard.ErrApprovalRequired):
		sreErrorCode(w, err.Error(), "approval_required", http.StatusForbidden)
	case errors.Is(err, grants.ErrRequestNotPending), errors.Is(err, grants.ErrNotActive):
		sreErrorCode(w, err.Error(), "conflict", http.StatusConflict)
	case errors.Is(err, agentguard.ErrUnavailable):
		sreErrorCode(w, err.Error(), "unavailable", http.StatusServiceUnavailable)
	default:
		internalError(w, r, "agent grants", err)
	}
}
