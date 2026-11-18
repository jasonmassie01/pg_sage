package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/review/modelgate"
	"github.com/pg-sage/sidecar/internal/review/scorecard"
)

// Sage Review's scorecard and model gate (review-05a):
//   - GET /api/v1/databases/{db}/review/scorecard?since=RFC3339 (viewer) is
//     the database's scorecard since then (default: the last 90 days), the
//     latest G-M1 and G-M2 evaluations and the thresholds.
//   - POST /api/v1/databases/{db}/review/gate/{id}/approve (admin) approves
//     the live G-M1 promotion: the model's review steps then run at their
//     class's trust level. Nothing else grants that.

const (
	reviewScorecardPath   = "/api/v1/databases/{db}/review/scorecard"
	reviewGateApprovePath = "/api/v1/databases/{db}/review/gate/{id}/approve"
	// DefaultScorecardWindow is the scorecard's window without since.
	DefaultScorecardWindow = 90 * 24 * time.Hour
)

// reviewScorecardResponse is the scorecard, the newest row of each gate
// rule (empty before the first evaluation) and the thresholds.
type reviewScorecardResponse struct {
	Scorecard  scorecard.Scorecard  `json:"scorecard"`
	Gate       []modelgate.Row      `json:"gate"`
	Thresholds modelgate.Thresholds `json:"thresholds"`
}

// gateSettings is review.gate, or its defaults without a config.
func gateSettings(cfg *config.Config) (modelgate.Thresholds, []string) {
	if cfg == nil {
		return modelgate.DefaultThresholds(), nil
	}
	return modelgate.ThresholdsFrom(cfg.Review.Gate), cfg.Review.Gate.FixtureDatabases
}

func registerReviewGateRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config) {
	route(mux, "GET "+reviewScorecardPath, gateViewer, reviewScorecardHandler(mgr, cfg))
	route(mux, "POST "+reviewGateApprovePath, gateAdmin, reviewGateApproveHandler(mgr))
}

func reviewScorecardHandler(mgr *fleet.DatabaseManager, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := packInstance(w, r, mgr)
		if !ok {
			return
		}
		since := time.Now().Add(-DefaultScorecardWindow)
		if raw := r.URL.Query().Get("since"); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				jsonError(w, "since must be an RFC 3339 time, got "+strconv.Quote(raw),
					http.StatusBadRequest)
				return
			}
			since = t
		}
		ctx, cancel := context.WithTimeout(r.Context(), reviewGradeReadTimeout)
		defer cancel()
		out, err := readScorecard(ctx, inst, cfg, since)
		if err != nil {
			slog.Error("review scorecard read failed", "database", inst.Name, "error", err)
			jsonError(w, "the review scorecard of "+inst.Name+" could not be read",
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, out)
	}
}

func readScorecard(ctx context.Context, inst *fleet.DatabaseInstance, cfg *config.Config,
	since time.Time) (reviewScorecardResponse, error) {
	th, fixtures := gateSettings(cfg)
	sc, err := scorecard.NewStore(inst.Pool).Read(ctx, inst.Name, since, fixtures)
	if err != nil {
		return reviewScorecardResponse{}, err
	}
	rows, err := modelgate.New(inst.Pool, inst.Name, modelgate.Options{}).Latest(ctx)
	if err != nil {
		return reviewScorecardResponse{}, err
	}
	return reviewScorecardResponse{Scorecard: sc, Gate: rows, Thresholds: th}, nil
}

func reviewGateApproveHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := packInstance(w, r, mgr)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			jsonError(w, "the gate evaluation id must be a positive integer",
				http.StatusBadRequest)
			return
		}
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		by := user.Email
		if by == "" {
			by = fmt.Sprintf("user:%d", user.ID)
		}
		row, err := modelgate.New(inst.Pool, inst.Name, modelgate.Options{}).
			Approve(r.Context(), id, by)
		if err != nil {
			writeGateError(w, inst.Name, id, err)
			return
		}
		slog.Info("review model gate approved", "database", inst.Name, "model", row.Model,
			"gate_id", row.ID, "approved_by", by)
		recordDecision(r, inbox.KindAutonomyPromotion, row.EvaluatedAt)
		jsonResponse(w, row)
	}
}

func writeGateError(w http.ResponseWriter, db string, id int64, err error) {
	switch {
	case errors.Is(err, modelgate.ErrNotFound):
		jsonError(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, modelgate.ErrNotPending), errors.Is(err, modelgate.ErrStale):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, modelgate.ErrInvalidInput):
		jsonError(w, err.Error(), http.StatusBadRequest)
	default:
		slog.Error("review model gate approval failed", "database", db, "gate_id", id,
			"error", err)
		jsonError(w, "the approval of gate evaluation "+strconv.FormatInt(id, 10)+
			" could not be recorded", http.StatusInternalServerError)
	}
}

// ReviewScorecardMCP serves MCP review_scorecard with the route's read.
func ReviewScorecardMCP(mgr *fleet.DatabaseManager, cfg *config.Config) mcp.
	ReviewScorecardBackend {
	return reviewScorecardMCP{mgr: mgr, cfg: cfg}
}

type reviewScorecardMCP struct {
	mgr *fleet.DatabaseManager
	cfg *config.Config
}

func (b reviewScorecardMCP) ReviewScorecard(ctx context.Context,
	req mcp.ReviewScorecardRequest) (any, error) {
	var inst *fleet.DatabaseInstance
	if b.mgr != nil {
		inst = b.mgr.GetInstance(req.Database)
	}
	if inst == nil || inst.Pool == nil {
		return nil, fmt.Errorf("database %q is not connected", req.Database)
	}
	since := time.Now().Add(-DefaultScorecardWindow)
	if req.Since != nil {
		since = *req.Since
	}
	ctx, cancel := context.WithTimeout(ctx, reviewGradeReadTimeout)
	defer cancel()
	return readScorecard(ctx, inst, b.cfg, since)
}
