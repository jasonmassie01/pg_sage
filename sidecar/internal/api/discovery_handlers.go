package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// registerDiscoveredRoutes registers the fleet discovery routes
// (market-06): the Discovered tab's list, adopt, ignore and extend-TTL.
// Admin only; every action is audited by the store.
func registerDiscoveredRoutes(mux *http.ServeMux, deps *DatabaseDeps) {
	route(mux, "GET /api/v1/databases/discovered", gateAdmin,
		listDiscoveredHandler(deps))
	route(mux, "POST /api/v1/databases/discovered/{id}/adopt", gateAdmin,
		adoptDiscoveredHandler(deps))
	route(mux, "POST /api/v1/databases/discovered/{id}/ignore", gateAdmin,
		ignoreDiscoveredHandler(deps))
	route(mux, "POST /api/v1/databases/discovered/{id}/extend", gateAdmin,
		extendDiscoveredHandler(deps))
}

func listDiscoveredHandler(deps *DatabaseDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := deps.Discovery.List(r.Context())
		if err != nil {
			internalError(w, r, "list discovered databases", err)
			return
		}
		sources, err := deps.Discovery.Sources(r.Context())
		if err != nil {
			internalError(w, r, "list discovery sources", err)
			return
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, discoveredToMap(row))
		}
		srcs := make([]map[string]any, 0, len(sources))
		for _, s := range sources {
			srcs = append(srcs, map[string]any{"source": s.Source,
				"last_run_at": s.LastRunAt, "last_success_at": s.LastSuccessAt,
				"last_error": s.LastError, "last_count": s.LastCount})
		}
		enrollment, err := enrollmentNumbers(r.Context(), deps.Discovery)
		if err != nil {
			internalError(w, r, "read enrollment numbers", err)
			return
		}
		jsonResponse(w, map[string]any{"discovered": out, "sources": srcs,
			"enrollment": enrollment})
	}
}

func discoveredToMap(r store.DiscoveredRow) map[string]any {
	tags := r.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	return map[string]any{"id": r.ID, "source": r.Source, "external_id": r.ExternalID,
		"host": r.Host, "port": r.Port, "dbname": r.DBName, "owner": r.Owner,
		"status": r.Status, "database_id": r.DatabaseID, "last_error": r.LastError,
		"first_seen": r.FirstSeen, "last_seen": r.LastSeen, "expires_at": r.ExpiresAt,
		"already_managed": r.AlreadyManaged,
		// fleetpol-01b: what the source knows and the enrollment milestones.
		"tags": tags, "creator": r.Creator, "created_at": r.CreatedAt,
		"set_name": r.SetName, "enrolled_at": r.EnrolledAt,
		"grade_requested_at": r.GradeRequestedAt, "first_graded_at": r.FirstGradedAt,
		"baseline_at": r.BaselineAt}
}

// enrollmentNumbers is each policy set's time to enrollment and 24 h
// baseline share (fleetpol-01b), the numbers behind
// pg_sage_fleet_enroll_seconds and pg_sage_fleet_baseline_24h_ratio.
func enrollmentNumbers(ctx context.Context, disc *store.DiscoveryStore) (
	[]map[string]any, error) {
	stats, err := disc.EnrollStats(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(stats))
	for _, s := range stats {
		item := map[string]any{"set": s.Set, "enrolled": s.Enrolled,
			"p50_seconds": s.P50Seconds, "p95_seconds": s.P95Seconds,
			"past_24h": s.Eligible, "baseline_24h": s.AtBaseline}
		if s.Eligible > 0 {
			item["baseline_24h_ratio"] = float64(s.AtBaseline) / float64(s.Eligible)
		}
		out = append(out, item)
	}
	return out, nil
}

func adoptDiscoveredHandler(deps *DatabaseDeps) http.HandlerFunc {
	return discoveredAction(func(ctx context.Context, id int64, userID int,
		_ *http.Request) (map[string]any, error) {
		return map[string]any{"status": store.DiscoveryAdopted},
			deps.Discovery.Adopt(ctx, id, userID)
	})
}

func ignoreDiscoveredHandler(deps *DatabaseDeps) http.HandlerFunc {
	return discoveredAction(func(ctx context.Context, id int64, userID int,
		_ *http.Request) (map[string]any, error) {
		return map[string]any{"status": store.DiscoveryIgnored},
			deps.Discovery.Ignore(ctx, id, userID)
	})
}

func extendDiscoveredHandler(deps *DatabaseDeps) http.HandlerFunc {
	return discoveredAction(func(ctx context.Context, id int64, userID int,
		r *http.Request) (map[string]any, error) {
		var body struct {
			Hours int `json:"hours"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, errors.Join(store.ErrValidation,
				errors.New("body must be {\"hours\": n}"))
		}
		expires, err := deps.Discovery.ExtendTTL(ctx, id, body.Hours, userID)
		return map[string]any{"expires_at": expires}, err
	})
}

type discoveredActionFunc func(ctx context.Context, id int64, userID int,
	r *http.Request) (map[string]any, error)

// discoveredAction parses the row id and maps store errors: validation and
// wrong state 400, missing row 404.
func discoveredAction(act discoveredActionFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			jsonError(w, "invalid discovered database ID", http.StatusBadRequest)
			return
		}
		userID := 0
		if u := UserFromContext(r.Context()); u != nil {
			userID = u.ID
		}
		out, err := act(r.Context(), id, userID, r)
		switch {
		case errors.Is(err, store.ErrValidation):
			jsonError(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, store.ErrNotFound):
			jsonError(w, "discovered database not found", http.StatusNotFound)
		case err != nil:
			internalError(w, r, "discovered database action", err)
		default:
			jsonResponse(w, out)
		}
	}
}
