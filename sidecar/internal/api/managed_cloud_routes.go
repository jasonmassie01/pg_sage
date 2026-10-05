package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Managed clouds (roadmap phase 3). GET /api/v1/managed-changes lists, per
// database (or ?database=), the typed parameter-group / database-flag
// proposals (?status=pending,approved,...; default all). POST
// /api/v1/managed-changes/{id}/approve|reject?database= records an
// operator's decision: approval means the operator applies the change,
// pg_sage never does. GET /api/v1/cloud-telemetry serves each database's
// host telemetry, its admission withhold reasons and parameter drift.

const managedChangesMeaning = "Settings SQL cannot change on RDS, Aurora or Cloud SQL " +
	"(restart-required or provider-restricted) are proposed as the exact parameter-group " +
	"or database-flag change, with the reboot it needs and its rollback. pg_sage never " +
	"applies them: approve when you will run the command, reject otherwise. pg_sage marks " +
	"a proposal applied once PostgreSQL runs the new value."

func registerManagedCloudRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	viewer := RequireRole("admin", "operator", "viewer")
	operator := RequireRole("admin", "operator")
	mux.Handle("GET /api/v1/managed-changes", viewer(managedChangesHandler(mgr)))
	mux.Handle("POST /api/v1/managed-changes/{id}/approve",
		operator(managedChangeDecisionHandler(mgr, true)))
	mux.Handle("POST /api/v1/managed-changes/{id}/reject",
		operator(managedChangeDecisionHandler(mgr, false)))
	mux.Handle("GET /api/v1/cloud-telemetry", viewer(cloudTelemetryHandler(mgr)))
}

type managedChangeDatabase struct {
	Database  string                `json:"database"`
	Proposals []managedparam.Record `json:"proposals"`
}

func managedChangesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		var statuses []string
		if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
			statuses = strings.Split(raw, ",")
		}
		for _, st := range statuses {
			if !validManagedStatus(st) {
				jsonError(w, "unknown status "+strconv.Quote(st), http.StatusBadRequest)
				return
			}
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		out := []managedChangeDatabase{}
		for _, np := range poolsForDatabaseSelection(mgr, database) {
			recs, err := managedparam.NewStore(np.pool).List(r.Context(), statuses, 200)
			if err != nil {
				internalError(w, r, "read managed changes of "+np.name, err)
				return
			}
			out = append(out, managedChangeDatabase{Database: np.name, Proposals: recs})
		}
		jsonResponse(w, map[string]any{"databases": out, "meaning": managedChangesMeaning})
	}
}

func validManagedStatus(st string) bool {
	switch st {
	case managedparam.StatusPending, managedparam.StatusApproved, managedparam.StatusRejected,
		managedparam.StatusApplied, managedparam.StatusSuperseded:
		return true
	}
	return false
}

func managedChangeDecisionHandler(mgr *fleet.DatabaseManager, approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "sign in to decide a managed change", http.StatusUnauthorized)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			jsonError(w, "invalid managed change id", http.StatusBadRequest)
			return
		}
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" || database == "all" {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		pool := mgr.PoolForDatabase(database)
		if pool == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		rec, err := managedparam.NewStore(pool).Decide(r.Context(), id, approve, user.ID,
			decisionNote(r))
		writeManagedDecision(w, r, rec, err)
	}
}

func decisionNote(r *http.Request) string {
	var body struct {
		Note string `json:"note"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil || len(raw) == 0 {
		return ""
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Note)
}

func writeManagedDecision(w http.ResponseWriter, r *http.Request, rec managedparam.Record,
	err error) {
	switch {
	case errors.Is(err, managedparam.ErrNotFound):
		jsonError(w, "managed change not found", http.StatusNotFound)
	case errors.Is(err, managedparam.ErrNotPending):
		jsonError(w, "managed change is no longer pending", http.StatusConflict)
	case err != nil:
		internalError(w, r, "decide managed change", err)
	default:
		jsonResponse(w, map[string]any{"id": rec.ID, "status": rec.Status,
			"decided_by": rec.DecidedBy, "decided_at": rec.DecidedAt,
			"decision_note": rec.DecisionNote, "proposal": rec.Proposal,
			"applied_by_pg_sage": false,
			"next_step": "run the command; pg_sage marks the change applied once " +
				"PostgreSQL runs the new value"})
	}
}

func cloudTelemetryHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		out := []cloudtel.Status{}
		for _, st := range cloudtel.Statuses() {
			if database == "" || database == "all" || st.Database == database {
				out = append(out, st)
			}
		}
		jsonResponse(w, map[string]any{"databases": out})
	}
}
