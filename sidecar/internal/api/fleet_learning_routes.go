package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/fleetlearn"
)

// FleetLearningReader serves fleet learning state the API cannot compute
// from the fleet alone: look-alikes (from the control database) and the
// leader election status.
type FleetLearningReader interface {
	LookAlikes(ctx context.Context, database string) (any, error)
	LeaderStatus() any
}

const (
	fleetFindingsPerDatabase = 500
	maxFleetFindingMinimum   = 10000
)

// registerFleetLearningRoutes registers the fleet learning read routes.
func registerFleetLearningRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config, reader FleetLearningReader) {
	viewer := RequireRole("admin", "operator", "viewer")
	mux.Handle("GET /api/v1/fleet/findings", viewer(fleetFindingsHandler(mgr, cfg)))
	mux.Handle("GET /api/v1/fleet/lookalikes", viewer(fleetLookalikesHandler(mgr, reader)))
	mux.Handle("GET /api/v1/fleet/leader", viewer(fleetLeaderHandler(reader)))
}

// FleetSources are the fleet's databases with a pool, sorted by name.
func FleetSources(mgr *fleet.DatabaseManager) []fleetlearn.Source {
	if mgr == nil {
		return nil
	}
	var out []fleetlearn.Source
	for name, inst := range mgr.Instances() {
		if inst != nil && inst.Pool != nil {
			out = append(out, fleetlearn.Source{Name: name, Pool: inst.Pool})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fleetFindingsHandler serves the problems open on several databases,
// each with its per-database drill-down. ?min_databases overrides
// fleet_learning.fleet_finding_min_databases.
func fleetFindingsHandler(mgr *fleet.DatabaseManager, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		minDBs := config.DefaultFleetFindingMinDatabases
		if cfg != nil {
			minDBs = cfg.FleetLearning.FleetFindingMinDatabases
		}
		if raw := r.URL.Query().Get("min_databases"); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < fleetlearn.MinFleetDatabases || v > maxFleetFindingMinimum {
				jsonError(w, "min_databases must be an integer from 2 to 10000",
					http.StatusBadRequest)
				return
			}
			minDBs = v
		}
		res := fleetlearn.CollectFleetFindings(r.Context(), FleetSources(mgr), minDBs,
			fleetFindingsPerDatabase)
		for _, e := range res.Errors {
			slog.Error("fleet read failed", "database", e.Database,
				"op", "fleet findings", "error", e.Cause)
		}
		jsonResponse(w, res)
	}
}

// fleetLookalikesHandler serves ?database's look-alike databases.
func fleetLookalikesHandler(mgr *fleet.DatabaseManager,
	reader FleetLearningReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			jsonError(w, "fleet learning is not running", http.StatusServiceUnavailable)
			return
		}
		db, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if db == "" || db == "all" {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, db) {
			return
		}
		looks, err := reader.LookAlikes(r.Context(), db)
		if errors.Is(err, fleetlearn.ErrDatabaseRequired) {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if err != nil {
			internalError(w, r, "fleet look-alikes", err)
			return
		}
		jsonResponse(w, map[string]any{"database": db, "lookalikes": looks})
	}
}

// fleetLeaderHandler serves the leader election status.
func fleetLeaderHandler(reader FleetLearningReader) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if reader == nil {
			jsonResponse(w, map[string]any{"enabled": false})
			return
		}
		jsonResponse(w, reader.LeaderStatus())
	}
}
