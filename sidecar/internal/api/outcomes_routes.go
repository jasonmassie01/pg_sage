package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/outcomes"
)

// The outcome ledger (ux-02a): GET /api/v1/outcomes?since=&database=
// &verdict=&evidence=&limit= lists one row per change pg_sage made in the
// window across the databases the caller may read, newest first, with a
// summary. Read-only. A database that could not be read is named in
// errors; its rows are missing, not empty.

const outcomesPath = "/api/v1/outcomes"

func registerOutcomesRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	routeAggregate(mux, "GET "+outcomesPath, gateViewer, mgr,
		func(m *fleet.DatabaseManager) http.Handler { return outcomesHandler(m) })
}

func parseOutcomesQuery(w http.ResponseWriter, r *http.Request) (string, outcomes.Query,
	bool) {
	v := r.URL.Query()
	db, ok := readDatabaseParam(w, r)
	if !ok {
		return "", outcomes.Query{}, false
	}
	q := outcomes.Query{Now: time.Now(), Evidence: v.Get("evidence")}
	var err error
	if q.Since, err = outcomes.ParseSince(v.Get("since")); err == nil {
		if q.Verdict, err = outcomes.ParseVerdict(v.Get("verdict")); err == nil {
			q.Limit, err = outcomes.ParseLimit(v.Get("limit"))
		}
	}
	if err == nil && len(q.Evidence) > 200 {
		err = errors.New("evidence is at most 200 characters")
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return "", outcomes.Query{}, false
	}
	return oneDatabase(db), q, true
}

func outcomesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, q, ok := parseOutcomesQuery(w, r)
		if !ok || rejectUnknownDatabase(w, mgr, db) {
			return
		}
		jsonResponse(w, readOutcomes(r.Context(), mgr, inboxDatabases(mgr, nil), db, q))
	}
}

// readOutcomes reads q from names (or from database alone) and merges
// them. A read error is logged in full and answered by database name only.
func readOutcomes(ctx context.Context, mgr *fleet.DatabaseManager, names []string,
	database string, q outcomes.Query) outcomes.Result {
	if database != "" {
		names = []string{database}
	}
	parts := make([]outcomes.Result, 0, len(names))
	for _, name := range names {
		var inst *fleet.DatabaseInstance
		if mgr != nil {
			inst = mgr.GetInstance(name)
		}
		if inst == nil || inst.Pool == nil {
			parts = append(parts, unreadOutcomes(name, "not connected"))
			continue
		}
		res, err := outcomes.Read(ctx, inst.Pool, name, q)
		if err != nil {
			slog.Error("outcome ledger read failed", "database", name, "error", err)
			parts = append(parts, unreadOutcomes(name, "the ledger could not be read"))
			continue
		}
		outcomes.Observe(name, q, res)
		parts = append(parts, res)
	}
	res := outcomes.Merge(q.Limit, parts...)
	res.Since = outcomes.FormatSince(q.Since)
	return res
}

func unreadOutcomes(name, why string) outcomes.Result {
	return outcomes.Result{Errors: []outcomes.ReadError{{Database: name,
		Error: "read outcomes: " + name + ": " + why}}}
}

// OutcomesMCP serves get_outcomes from the same read as the route.
func OutcomesMCP(mgr *fleet.DatabaseManager) mcp.OutcomesBackend {
	return outcomesMCP{mgr: mgr}
}

type outcomesMCP struct{ mgr *fleet.DatabaseManager }

func (b outcomesMCP) Outcomes(ctx context.Context, req mcp.OutcomesRequest) (any, error) {
	names := inboxDatabases(b.mgr, req.Databases)
	if req.Database != "" && !contains(names, req.Database) {
		return nil, mcp.ErrOutcomesDatabaseNotFound
	}
	q := outcomes.Query{Since: req.Since, Now: time.Now(), Verdict: req.Verdict,
		Evidence: req.Evidence, Limit: req.Limit}
	if q.Since == 0 {
		q.Since = outcomes.DefaultSince
	}
	if q.Limit == 0 {
		q.Limit = outcomes.DefaultLimit
	}
	return readOutcomes(ctx, b.mgr, names, req.Database, q), nil
}
