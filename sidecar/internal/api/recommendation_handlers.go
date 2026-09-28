package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/store"
)

// registerRecommendationRoutes mounts the read-only recommendation API.
func registerRecommendationRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	mux.HandleFunc("GET /api/v1/recommendations", recommendationsListHandler(mgr))
	mux.HandleFunc("GET /api/v1/recommendations/{id}", recommendationDetailHandler(mgr))
}

// recommendationScope resolves the one database a recommendation request
// reads. It writes the error response and returns false when it cannot.
func recommendationScope(
	w http.ResponseWriter, r *http.Request, mgr *fleet.DatabaseManager,
) (namedPool, bool) {
	dbName, ok := readDatabaseParam(w, r)
	if !ok {
		return namedPool{}, false
	}
	selected, ok := resolveSingleDatabaseRequestPool(mgr, dbName)
	if !ok {
		jsonError(w, "database is required", http.StatusBadRequest)
		return namedPool{}, false
	}
	if selected.pool == nil {
		jsonError(w, "database not found", http.StatusNotFound)
		return namedPool{}, false
	}
	return selected, true
}

// recommendationsListHandler lists one database's recommendations, newest
// first, optionally filtered by state. Read-only.
func recommendationsListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		selected, ok := recommendationScope(w, r, mgr)
		if !ok {
			return
		}
		q := r.URL.Query()
		state := recommendation.State(q.Get("state"))
		if state != "" && !state.Valid() {
			jsonError(w, "unknown recommendation state", http.StatusBadRequest)
			return
		}
		recs, err := recommendation.NewStore(selected.pool).List(r.Context(),
			recommendation.ListFilter{DatabaseName: selected.name, State: state,
				Limit: parseIntDefault(q.Get("limit"), 100)})
		if err != nil {
			slog.Error("list recommendations failed", "database", selected.name,
				"error", err)
			jsonError(w, "failed to list recommendations", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{
			"database": selected.name, "recommendations": recs,
		})
	}
}

// recommendationDetailHandler returns one recommendation with every
// revision and its full transition history. Read-only.
func recommendationDetailHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			jsonError(w, "invalid recommendation id", http.StatusBadRequest)
			return
		}
		selected, ok := recommendationScope(w, r, mgr)
		if !ok {
			return
		}
		recs := recommendation.NewStore(selected.pool)
		head, err := recs.Get(r.Context(), id)
		if errors.Is(err, recommendation.ErrNotFound) ||
			(err == nil && head.DatabaseName != selected.name) {
			jsonError(w, "recommendation not found", http.StatusNotFound)
			return
		}
		revisions, revErr := recs.Revisions(r.Context(), id)
		history, histErr := recs.Transitions(r.Context(), id)
		if err = errors.Join(err, revErr, histErr); err != nil {
			slog.Error("get recommendation failed", "id", id, "error", err)
			jsonError(w, "failed to get recommendation", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{
			"database": selected.name, "recommendation": head,
			"revisions": revisions, "transitions": history,
		})
	}
}

// approveFailure answers a refused approval. A proposal whose
// recommendation was revised, or is no longer awaiting approval, is a
// conflict: the operator must review the current revision (C04).
func approveFailure(w http.ResponseWriter, queueID int, err error) {
	slog.Error("approve action failed", "action_id", queueID, "error", err)
	switch {
	case errors.Is(err, recommendation.ErrRevised):
		jsonError(w, "the recommendation was revised after this proposal; "+
			"review and approve its current revision", http.StatusConflict)
	case errors.Is(err, recommendation.ErrConflict):
		jsonError(w, "the recommendation is no longer awaiting approval",
			http.StatusConflict)
	default:
		jsonError(w, "failed to approve action", http.StatusNotFound)
	}
}

// addRecommendationFields exposes the revision a queued proposal is
// pinned to.
func addRecommendationFields(m map[string]any, a store.QueuedAction) {
	if a.RecommendationID == nil {
		return
	}
	m["recommendation_id"] = *a.RecommendationID
	m["recommendation_revision"] = a.RecommendationRevision
	m["content_hash"] = a.ContentHash
}
