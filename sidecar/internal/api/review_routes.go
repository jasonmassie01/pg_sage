package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/review/report"
	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/review/trigger"
)

// Sage Review's grade (review-01a), read-only:
//   - GET /api/v1/databases/{db}/review/grade is the latest graded run of
//     the database (done or partial), the one before it, and the delta
//     between them; the first grade's delta is the full report. Before
//     the first grade, latest is null. why_now is the triggers the latest
//     run consumed, with their details (review-04a; empty for a grade).
//
// The Review page (review-01b) reads three more fields: running (the run
// in progress, polled until it finishes), report (the latest grade's
// templated report) and viewed_at (when the caller first opened the latest
// grade; review_view_routes.go records it).
//
// Runs live in the database's own sage schema (sage.review_run and
// sage.review_grade). The route never starts a grade.

const (
	reviewGradePath        = "/api/v1/databases/{db}/review/grade"
	reviewGradeReadTimeout = 10 * time.Second
)

type reviewGradeResponse struct {
	Latest   *rubric.Run       `json:"latest"`
	Previous *rubric.Run       `json:"previous"`
	Delta    *rubric.Changes   `json:"delta"`
	Running  *reviewRunning    `json:"running"`
	Report   *report.RunReport `json:"report"`
	ViewedAt *time.Time        `json:"viewed_at"`
	// WhyNow is the triggers the latest run consumed or coalesced, oldest
	// first: their kinds and details (review-04a).
	WhyNow []trigger.Trigger `json:"why_now"`
}

// reviewRunning is the run in progress, shown as a banner.
type reviewRunning struct {
	ID        int64       `json:"id"`
	Kind      rubric.Kind `json:"kind"`
	StartedAt time.Time   `json:"started_at"`
}

// registerReviewRoutes registers the grade route and the read receipt on
// the per-database mux (perDatabaseSREMux), beside the other "{db}/..."
// routes. The receipt is scoped like a read of the database.
func registerReviewRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	route(mux, "GET "+reviewGradePath, gateViewer, reviewGradeHandler(mgr))
	route(mux, "POST "+reviewViewedPath, gateViewerWrite,
		requireReadScope(readDatabase(pathDB))(reviewViewedHandler(mgr)))
}

func reviewGradeHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := packInstance(w, r, mgr)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), reviewGradeReadTimeout)
		defer cancel()
		store := rubric.NewStore(inst.Pool)
		out, err := readReviewGrade(ctx, inst.Pool, inst.Name)
		if err != nil {
			slog.Error("review grade read failed", "database", inst.Name, "error", err)
			jsonError(w, "the review grade of "+inst.Name+" could not be read",
				http.StatusInternalServerError)
			return
		}
		out.ViewedAt = callerViewedAt(ctx, r, store, out.Latest)
		jsonResponse(w, out)
	}
}

// readReviewGrade is database's two newest graded runs, their delta, the
// latest one's report and triggers, and the run in progress.
func readReviewGrade(ctx context.Context, pool *pgxpool.Pool, database string) (
	reviewGradeResponse, error) {
	s := rubric.NewStore(pool)
	runs, err := s.Latest(ctx, database, 2)
	if err != nil {
		return reviewGradeResponse{}, err
	}
	running, err := s.Running(ctx, database)
	if err != nil {
		return reviewGradeResponse{}, err
	}
	out := buildReviewGrade(runs, running)
	if out.Latest == nil {
		return out, nil
	}
	why, err := trigger.NewStore(pool).WhyNow(ctx, out.Latest.ID)
	if err != nil {
		return reviewGradeResponse{}, err
	}
	if why != nil {
		out.WhyNow = why
	}
	return out, nil
}

// buildReviewGrade is the response of runs (newest first; two are read)
// and the running run, if any; why_now is empty until readReviewGrade
// reads the latest run's triggers.
func buildReviewGrade(runs []rubric.Run, running *rubric.Run) reviewGradeResponse {
	out := reviewGradeResponse{WhyNow: []trigger.Trigger{}}
	if running != nil {
		out.Running = &reviewRunning{ID: running.ID, Kind: running.Kind,
			StartedAt: running.StartedAt.UTC()}
	}
	if len(runs) == 0 {
		return out
	}
	out.Latest = &runs[0]
	var prev *rubric.Grade
	if len(runs) > 1 {
		out.Previous, prev = &runs[1], &runs[1].Grade
	}
	delta := rubric.Delta(prev, runs[0].Grade)
	out.Delta = &delta
	rep := report.FromRun(runs[0])
	out.Report = &rep
	return out
}

// callerViewedAt is when the signed-in caller first opened latest, or nil.
// A failed read is logged and left nil: the receipt never blocks the page,
// whose own POST then reports the failure.
func callerViewedAt(ctx context.Context, r *http.Request, s *rubric.Store,
	latest *rubric.Run) *time.Time {
	user := UserFromContext(r.Context())
	if latest == nil || user == nil || user.ID <= 0 {
		return nil
	}
	at, err := s.ViewedAt(ctx, latest.ID, user.ID)
	if err != nil {
		slog.Warn("review read receipt lookup failed", "run_id", latest.ID,
			"user_id", user.ID, "error", err)
		return nil
	}
	return at
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
	return readReviewGrade(ctx, inst.Pool, inst.Name)
}
