package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// The Review page's read receipt (review-01b): POST
// /api/v1/databases/{db}/review/runs/{id}/viewed records the caller's
// first opening of a finished review in the database's sage.review_view,
// once per run and user, and answers {run_id, viewed_at, first}. It is the
// evidence that a person read a review. The run id is per database (each
// database keeps its own sage.review_run), so the path names the database.
//
// Errors: 400 for an id that is not a positive integer, 401 without a
// user id, 404 for an unknown database or run, 409 for a run that is not a
// finished review (running or failed), 503 for a disconnected database,
// and a generic 500 (the pgx text is logged, never sent).

const reviewViewedPath = "/api/v1/databases/{db}/review/runs/{id}/viewed"

func reviewViewedHandler(mgr *fleet.DatabaseManager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil || user.ID <= 0 {
			jsonError(w, "a read receipt is kept for a signed-in user",
				http.StatusUnauthorized)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			jsonError(w, "run id must be a positive integer", http.StatusBadRequest)
			return
		}
		inst, ok := packInstance(w, r, mgr)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), reviewGradeReadTimeout)
		defer cancel()
		v, err := rubric.NewStore(inst.Pool).MarkViewed(ctx, inst.Name, id, user.ID)
		if err != nil {
			viewedFailed(w, inst.Name, id, user.ID, err)
			return
		}
		jsonResponse(w, v)
	})
}

func viewedFailed(w http.ResponseWriter, database string, id int64, userID int, err error) {
	run := "review run " + strconv.FormatInt(id, 10)
	switch {
	case errors.Is(err, rubric.ErrRunNotFound):
		jsonError(w, run+" of "+database+" not found", http.StatusNotFound)
	case errors.Is(err, rubric.ErrNotFinished):
		jsonError(w, run+" of "+database+" is not a finished review", http.StatusConflict)
	default:
		slog.Error("review read receipt failed", "database", database, "run_id", id,
			"user_id", userID, "error", err)
		jsonError(w, "the read receipt of "+run+" could not be recorded",
			http.StatusInternalServerError)
	}
}
