package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// registerSourceFixRoutes serves a finding's source-fix report (mcp-06):
// the pull request and deploy a coding agent reported, and pg_sage's
// verdict. Read-only: the route never decides a verdict.
func registerSourceFixRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config) {
	routeAggregate(mux, "GET /api/v1/findings/{id}/source-fix", gateViewer, mgr,
		func(m *fleet.DatabaseManager) http.Handler { return sourceFixReportHandler(m, cfg) })
}

func sourceFixReportHandler(mgr *fleet.DatabaseManager, cfg *config.Config,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			jsonError(w, "finding id must be a positive integer", http.StatusBadRequest)
			return
		}
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, dbName)
		if !ok {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		tools := agenttools.New(selected.pool,
			agenttools.Options{VerifyWindow: sourceFixVerifyWindow(cfg)})
		report, err := tools.SourceFixReport(r.Context(), id)
		if errors.Is(err, agenttools.ErrNotFound) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "no source-fix report for this finding", "code": "no_source_fix"})
			return
		}
		if err != nil {
			internalError(w, r, "read source-fix report", err)
			return
		}
		jsonResponse(w, report)
	}
}

// sourceFixVerifyWindow is verify.window_minutes, the window the
// verifier decides with; 0 means the agent tools' default.
func sourceFixVerifyWindow(cfg *config.Config) time.Duration {
	if cfg == nil {
		return 0
	}
	return time.Duration(cfg.Verify.WindowMinutes) * time.Minute
}
