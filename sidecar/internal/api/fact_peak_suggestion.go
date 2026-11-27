package api

import (
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// GET /api/v1/databases/{db}/facts/peak-suggestion (evid-03b) computes the
// database's peak-window suggestion from its hourly query rollup on
// request; nothing is stored. An operator applies it by declaring the
// owner brief with it (POST /api/v1/facts). "suggestion" is
// "peak_windows" with the value, or "none" with the reason.

const factPeakSuggestionPath = "/api/v1/databases/{db}/facts/peak-suggestion"

// peakSuggestionView is a suggestion as served.
type peakSuggestionView struct {
	Database   string `json:"database"`
	Suggestion string `json:"suggestion"`
	facts.PeakSuggestion
}

func registerFactPeakRoute(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	route(mux, "GET "+factPeakSuggestionPath, gateViewer, factPeakSuggestionHandler(mgr))
}

func factPeakSuggestionHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("db")
		if name == "" || name == "all" || validateDatabaseParam(name) != nil {
			sreErrorCode(w, "invalid database name", "invalid_request", http.StatusBadRequest)
			return
		}
		var inst *fleet.DatabaseInstance
		if mgr != nil {
			inst = mgr.GetInstance(name)
		}
		if inst == nil || inst.Pool == nil {
			sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
			return
		}
		got, err := facts.SuggestPeakWindows(r.Context(), inst.Pool, time.Now())
		if err != nil {
			internalError(w, r, "peak-window suggestion", err)
			return
		}
		view := peakSuggestionView{Database: inst.Name, Suggestion: "none",
			PeakSuggestion: got}
		if got.PeakWindows != "" {
			view.Suggestion = "peak_windows"
		}
		jsonResponse(w, view)
	}
}
