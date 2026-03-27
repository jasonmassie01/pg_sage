package api

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// NewRouter creates the API + dashboard HTTP handler.
func NewRouter(
	mgr *fleet.DatabaseManager,
	cfg *config.Config,
) http.Handler {
	mux := http.NewServeMux()

	// Fleet overview
	mux.HandleFunc("GET /api/v1/databases", databasesHandler(mgr))

	// Findings
	mux.HandleFunc("GET /api/v1/findings", findingsListHandler(mgr))
	mux.HandleFunc("GET /api/v1/findings/{id}",
		findingDetailHandler(mgr))
	mux.HandleFunc("POST /api/v1/findings/{id}/suppress",
		suppressHandler(mgr))
	mux.HandleFunc("POST /api/v1/findings/{id}/unsuppress",
		unsuppressHandler(mgr))

	// Actions
	mux.HandleFunc("GET /api/v1/actions", actionsListHandler(mgr))
	mux.HandleFunc("GET /api/v1/actions/{id}",
		actionDetailHandler(mgr))

	// Forecasts, Query Hints, Alert Log
	mux.HandleFunc("GET /api/v1/forecasts",
		forecastsHandler(mgr))
	mux.HandleFunc("GET /api/v1/query-hints",
		queryHintsHandler(mgr))
	mux.HandleFunc("GET /api/v1/alert-log",
		alertLogHandler(mgr))

	// Snapshots
	mux.HandleFunc("GET /api/v1/snapshots/latest",
		snapshotLatestHandler(mgr))
	mux.HandleFunc("GET /api/v1/snapshots/history",
		snapshotHistoryHandler(mgr))

	// Config
	mux.HandleFunc("GET /api/v1/config",
		configGetHandler(mgr, cfg))
	mux.HandleFunc("PUT /api/v1/config",
		configUpdateHandler(mgr, cfg))

	// Metrics (JSON)
	mux.HandleFunc("GET /api/v1/metrics", metricsHandler(mgr))

	// Emergency controls
	mux.HandleFunc("POST /api/v1/emergency-stop",
		emergencyStopHandler(mgr))
	mux.HandleFunc("POST /api/v1/resume", resumeHandler(mgr))

	// Embedded dashboard (SPA fallback)
	staticSub, _ := fs.Sub(staticFiles, "dist")
	fileServer := http.FileServer(http.FS(staticSub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve static file if it exists, otherwise index.html for SPA
		path := r.URL.Path
		if path == "/" || !strings.Contains(path, ".") {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})

	return corsMiddleware(mux)
}
