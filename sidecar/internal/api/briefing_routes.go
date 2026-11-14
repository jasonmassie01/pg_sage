package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Briefings API (llm-06b): GET /api/v1/briefings?database=&limit= lists
// stored briefings newest first; GET /api/v1/briefings/latest?database=
// serves each database's latest one. content_json and delivery_status are
// returned exactly as stored. Every signed-in role reads them.

const briefingsPath = "/api/v1/briefings"

// briefingReader reads one database's stored briefings (briefing.Store).
type briefingReader interface {
	List(ctx context.Context, limit int) ([]briefing.Record, error)
	Latest(ctx context.Context) (briefing.Record, error)
}

type briefingSource struct {
	name   string
	reader briefingReader
}

// briefingDeps resolves ?database= to the readers it selects; false is an
// unknown database.
type briefingDeps struct {
	sources func(database string) ([]briefingSource, bool)
}

func registerBriefingRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	routeAggregate(mux, "GET "+briefingsPath, gateViewer, mgr,
		func(m *fleet.DatabaseManager) http.Handler {
			return briefingsListHandler(fleetBriefingDeps(m))
		})
	routeAggregate(mux, "GET "+briefingsPath+"/latest", gateViewer, mgr,
		func(m *fleet.DatabaseManager) http.Handler {
			return briefingsLatestHandler(fleetBriefingDeps(m))
		})
}

func registerBriefingRoutesWith(mux *http.ServeMux, deps briefingDeps) {
	route(mux, "GET "+briefingsPath, gateViewer, briefingsListHandler(deps))
	route(mux, "GET "+briefingsPath+"/latest", gateViewer, briefingsLatestHandler(deps))
}

// fleetBriefingDeps reads each monitored database's sage.briefings.
func fleetBriefingDeps(mgr *fleet.DatabaseManager) briefingDeps {
	return briefingDeps{sources: func(database string) ([]briefingSource, bool) {
		if mgr != nil && database != "" && database != "all" &&
			mgr.GetInstance(database) == nil {
			return nil, false
		}
		out := []briefingSource{}
		for _, np := range poolsForDatabaseSelection(mgr, database) {
			out = append(out, briefingSource{name: np.name, reader: briefing.NewStore(np.pool)})
		}
		return out, true
	}}
}

// selectBriefingSources answers the shared part of both routes: the
// database parameter, an unknown database and no connected database.
func selectBriefingSources(w http.ResponseWriter, r *http.Request,
	deps briefingDeps) (string, []briefingSource, bool) {
	database, ok := readDatabaseParam(w, r)
	if !ok {
		return "", nil, false
	}
	var sources []briefingSource
	known := true
	if deps.sources != nil {
		sources, known = deps.sources(database)
	}
	if !known {
		jsonError(w, "database not found", http.StatusNotFound)
		return "", nil, false
	}
	if len(sources) == 0 {
		jsonError(w, "briefings unavailable: no connected database", http.StatusServiceUnavailable)
		return "", nil, false
	}
	return database, sources, true
}

// briefingsPage is one answer. Unavailable names the databases that failed
// in a fleet read; the others still answer.
type briefingsPage struct {
	Briefings       []briefing.Record `json:"briefings"`
	WithoutBriefing []string          `json:"without_briefing,omitempty"`
	Unavailable     []string          `json:"unavailable,omitempty"`
}

func briefingsListHandler(deps briefingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, err := briefing.ParseLimit(r.URL.Query().Get("limit"))
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, sources, ok := selectBriefingSources(w, r, deps)
		if !ok {
			return
		}
		page := briefingsPage{Briefings: []briefing.Record{}}
		var firstErr error
		for _, src := range sources {
			list, err := src.reader.List(r.Context(), limit)
			if err != nil {
				firstErr = noteBriefingFailure(&page, src.name, err, firstErr)
				continue
			}
			for _, rec := range list {
				rec.Database = src.name
				page.Briefings = append(page.Briefings, rec)
			}
		}
		if len(page.Unavailable) == len(sources) {
			internalError(w, r, "list briefings", firstErr)
			return
		}
		sortNewestFirst(page.Briefings)
		if len(page.Briefings) > limit {
			page.Briefings = page.Briefings[:limit]
		}
		jsonResponse(w, page)
	}
}

func briefingsLatestHandler(deps briefingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, sources, ok := selectBriefingSources(w, r, deps)
		if !ok {
			return
		}
		page, firstErr := latestBriefings(r.Context(), sources)
		if len(page.Unavailable) == len(sources) {
			internalError(w, r, "read the latest briefing", firstErr)
			return
		}
		if len(page.Briefings) == 0 && len(page.Unavailable) == 0 {
			jsonError(w, noBriefingMessage(database), http.StatusNotFound)
			return
		}
		jsonResponse(w, page)
	}
}

func latestBriefings(ctx context.Context, sources []briefingSource) (briefingsPage, error) {
	page := briefingsPage{Briefings: []briefing.Record{}}
	var firstErr error
	for _, src := range sources {
		rec, err := src.reader.Latest(ctx)
		switch {
		case errors.Is(err, briefing.ErrNoBriefing):
			page.WithoutBriefing = append(page.WithoutBriefing, src.name)
		case err != nil:
			firstErr = noteBriefingFailure(&page, src.name, err, firstErr)
		default:
			rec.Database = src.name
			page.Briefings = append(page.Briefings, rec)
		}
	}
	return page, firstErr
}

// noteBriefingFailure records a database whose read failed (logged here,
// never echoed to the caller) and returns the first failure.
func noteBriefingFailure(page *briefingsPage, name string, err, first error) error {
	slog.Warn("api: read briefings", "database", name, "err", err)
	page.Unavailable = append(page.Unavailable, name)
	if first == nil {
		return err
	}
	return first
}

func noBriefingMessage(database string) string {
	if database == "" || database == "all" {
		return "no briefing yet: " + briefing.NoBriefingReason
	}
	return fmt.Sprintf("no briefing yet for database %q: %s", database,
		briefing.NoBriefingReason)
}

// sortNewestFirst orders briefings by generated_at, newest first, then by
// database and id so equal times have one order.
func sortNewestFirst(list []briefing.Record) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.GeneratedAt.Equal(b.GeneratedAt) {
			return a.GeneratedAt.After(b.GeneratedAt)
		}
		if a.Database != b.Database {
			return a.Database < b.Database
		}
		return a.ID > b.ID
	})
}
