package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/shadow"
)

// Shadow mode (roadmap 1.4): GET /api/v1/shadow-decisions serves, per
// database (or ?database=), what pg_sage would have done below each
// class's earned level and how it scored: a per-class summary and the
// newest decisions (?class=, ?status=, ?score=, ?limit= 1-1000, default
// 50). Every signed-in role reads it.

const shadowPath = "/api/v1/shadow-decisions"

const shadowMeaning = "Below a class's earned trust level pg_sage records every action " +
	"it would have taken (SQL, rollback, prediction, the gate's verdict had the class " +
	"been trusted) and never runs it. Each is scored later from what happened: the " +
	"operator's decision on the same proposal, the same change applied by anyone, or a " +
	"HypoPG what-if for index creates; unscored when nothing applies. Scores from a " +
	"change applied outside pg_sage or a what-if count toward promotion as shadow " +
	"evidence (L2 may rest on it; L3 needs at least 3 real verified outcomes); an " +
	"admin still approves every promotion."

func registerShadowRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	viewer := RequireRole("admin", "operator", "viewer")
	mux.Handle("GET "+shadowPath, viewer(shadowDecisionsHandler(mgr)))
}

// shadowDatabase is one database's shadow ledger as served.
type shadowDatabase struct {
	Database  string                `json:"database"`
	Summary   []shadow.ClassSummary `json:"summary"`
	Decisions []shadow.Decision     `json:"decisions"`
}

func shadowDecisionsHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		filter, err := parseShadowFilter(r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		out := []shadowDatabase{}
		for _, np := range poolsForDatabaseSelection(mgr, database) {
			s := shadow.NewStore(np.pool)
			db := shadowDatabase{Database: np.name}
			if db.Summary, err = s.Summary(r.Context()); err == nil {
				db.Decisions, err = s.List(r.Context(), filter)
			}
			if err != nil {
				internalError(w, r, "read shadow decisions of "+np.name, err)
				return
			}
			out = append(out, db)
		}
		jsonResponse(w, map[string]any{"databases": out, "meaning": shadowMeaning})
	}
}

func parseShadowFilter(r *http.Request) (shadow.Filter, error) {
	q := r.URL.Query()
	f := shadow.Filter{Class: q.Get("class"), Status: q.Get("status"),
		Score: q.Get("score"), Limit: 50}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return f, errors.New("limit must be an integer 1-1000")
		}
		f.Limit = n
	}
	return f, f.Validate()
}

// trustDatabaseView is a database's Trust view with its shadow summary
// per class (roadmap 1.4), so the Trust page reads one endpoint.
type trustDatabaseView struct {
	earned.TrustView
	Shadow []shadow.ClassSummary `json:"shadow"`
}

// withShadow adds the database's shadow summary to its Trust view; a
// summary that cannot be read is logged and served empty (the ledger
// view stands on its own).
func withShadow(ctx context.Context, mgr *fleet.DatabaseManager,
	v earned.TrustView) trustDatabaseView {
	out := trustDatabaseView{TrustView: v, Shadow: []shadow.ClassSummary{}}
	if mgr == nil {
		return out
	}
	pool := mgr.PoolForDatabase(v.Database)
	if pool == nil {
		return out
	}
	sum, err := shadow.NewStore(pool).Summary(ctx)
	if err != nil {
		slog.Warn("trust view: read the shadow summary", "database", v.Database,
			"error", err)
		return out
	}
	out.Shadow = sum
	return out
}
