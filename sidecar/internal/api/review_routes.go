package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// Sage Review's grade (review-01a), read-only:
//   - GET /api/v1/databases/{db}/review/grade is the latest graded run of
//     the database (done or partial), the one before it, and the delta
//     between them; the first grade's delta is the full report. Before
//     the first grade, latest is null.
//
// Runs live in the database's own sage schema (sage.review_run and
// sage.review_grade). The route never starts a grade.

const (
	reviewGradePath        = "/api/v1/databases/{db}/review/grade"
	reviewGradeReadTimeout = 10 * time.Second
)

type reviewGradeResponse struct {
	Latest   *rubric.Run     `json:"latest"`
	Previous *rubric.Run     `json:"previous"`
	Delta    *rubric.Changes `json:"delta"`
}

// registerReviewRoutes registers the grade route on the per-database mux
// (perDatabaseSREMux), beside the other "{db}/..." routes.
func registerReviewRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	route(mux, "GET "+reviewGradePath, gateViewer, reviewGradeHandler(mgr))
}

func reviewGradeHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := packInstance(w, r, mgr)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), reviewGradeReadTimeout)
		defer cancel()
		out, err := readReviewGrade(ctx, rubric.NewStore(inst.Pool), inst.Name)
		if err != nil {
			slog.Error("review grade read failed", "database", inst.Name, "error", err)
			jsonError(w, "the review grade of "+inst.Name+" could not be read",
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, out)
	}
}

// readReviewGrade is database's two newest graded runs and their delta.
func readReviewGrade(ctx context.Context, s *rubric.Store, database string) (
	reviewGradeResponse, error) {
	runs, err := s.Latest(ctx, database, 2)
	if err != nil || len(runs) == 0 {
		return reviewGradeResponse{}, err
	}
	out := reviewGradeResponse{Latest: &runs[0]}
	var prev *rubric.Grade
	if len(runs) > 1 {
		out.Previous, prev = &runs[1], &runs[1].Grade
	}
	delta := rubric.Delta(prev, runs[0].Grade)
	out.Delta = &delta
	return out, nil
}

// ReviewGradeMCP serves MCP review_grade with the route's read.
func ReviewGradeMCP(mgr *fleet.DatabaseManager) mcp.ReviewGradeBackend {
	return reviewGradeMCP{mgr: mgr}
}

type reviewGradeMCP struct{ mgr *fleet.DatabaseManager }

func (b reviewGradeMCP) ReviewGrade(ctx context.Context, req mcp.ReviewGradeRequest) (any,
	error) {
	var inst *fleet.DatabaseInstance
	if b.mgr != nil {
		inst = b.mgr.GetInstance(req.Database)
	}
	if inst == nil || inst.Pool == nil {
		return nil, fmt.Errorf("database %q is not connected", req.Database)
	}
	ctx, cancel := context.WithTimeout(ctx, reviewGradeReadTimeout)
	defer cancel()
	return readReviewGrade(ctx, rubric.NewStore(inst.Pool), inst.Name)
}
