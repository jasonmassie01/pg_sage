package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// GET /api/v1/upgrade-readiness?database=&target_major=&method=
// &target_database= (market-05) checks one monitored database for a
// major-version upgrade and returns each check's verdict. Any signed-in
// role may call it: every read is a bounded catalog read in one read-only
// transaction, at most upgradeReadinessSlots run at once, and nothing is
// written (the daily monitor, not this route, opens findings).
// target_database names a fleet member already at the target major whose
// available extensions U2 compares against.

const upgradeReadinessPath = "/api/v1/upgrade-readiness"

const (
	upgradeReadinessSlots  = 4
	upgradeReadinessBudget = 30 * time.Second
)

var errUnknownTargetDatabase = errors.New("target_database is not a monitored database")

// upgradeRunner is upgradecheck.Run; tests replace it.
type upgradeRunner func(context.Context, *pgxpool.Pool, upgradecheck.Target,
	upgradecheck.Options) (upgradecheck.Report, error)

type upgradeReadinessDeps struct {
	// pool resolves the database parameter like the migration lint does.
	pool func(database string) (*pgxpool.Pool, error)
	// member is a named fleet member's pool (target_database).
	member func(name string) (*pgxpool.Pool, error)
	// provider is the database parameter's provider, for U7.
	provider func(database string) string
	run      upgradeRunner
	slots    chan struct{}
	// trigger publishes the pre_upgrade review trigger (nil: none).
	trigger upgradeTriggerFunc
}

func registerUpgradeReadinessRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config) {
	deps := upgradeReadinessDeps{
		pool: func(database string) (*pgxpool.Pool, error) {
			return fleetDatabasePool(mgr, database)
		},
		member: func(name string) (*pgxpool.Pool, error) {
			if mgr == nil || mgr.GetInstance(name) == nil {
				return nil, errUnknownTargetDatabase
			}
			if pool := mgr.PoolForDatabase(name); pool != nil {
				return pool, nil
			}
			return nil, errLintNoDatabase
		},
		provider: func(database string) string {
			return memberProvider(mgr, cfg, database)
		},
		run:     upgradecheck.Run,
		slots:   make(chan struct{}, upgradeReadinessSlots),
		trigger: fleetUpgradeTrigger(mgr),
	}
	route(mux, "GET "+upgradeReadinessPath, gateViewer, upgradeReadinessHandler(deps))
}

type upgradeReadinessQuery struct {
	database, targetDatabase string
	target                   upgradecheck.Target
}

// memberProvider is the database's detected provider: each fleet member
// has its own, so the process's cloud environment is only the fallback
// for a member that reports none. database "" is the only member.
func memberProvider(mgr *fleet.DatabaseManager, cfg *config.Config, database string) string {
	if mgr != nil {
		if database == "" {
			if names := mgr.Instances(); len(names) == 1 {
				for name := range names {
					database = name
				}
			}
		}
		if inst := mgr.GetInstance(database); inst != nil {
			if p := inst.SnapshotStatus().Platform; p != "" {
				return p
			}
		}
	}
	if cfg == nil {
		return ""
	}
	config.RLockForHotReload()
	defer config.RUnlockForHotReload()
	return cfg.CloudEnvironment
}

// parseUpgradeReadiness reads and checks the query parameters.
func parseUpgradeReadiness(r *http.Request) (upgradeReadinessQuery, error) {
	q := r.URL.Query()
	out := upgradeReadinessQuery{database: q.Get("database"),
		targetDatabase: q.Get("target_database")}
	if err := validateDatabaseParam(out.database); err != nil {
		return out, err
	}
	if err := validateDatabaseParam(out.targetDatabase); err != nil {
		return out, fmt.Errorf("target_database: %w", err)
	}
	raw := q.Get("target_major")
	major, err := strconv.Atoi(raw)
	if raw == "" || err != nil {
		return out, fmt.Errorf("%w: target_major is required and must be an integer "+
			"major such as 18", upgradecheck.ErrInvalidTarget)
	}
	out.target = upgradecheck.Target{Major: major, Method: q.Get("method")}.Normalized()
	return out, out.target.ValidateStatic()
}

func upgradeReadinessHandler(d upgradeReadinessDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := parseUpgradeReadiness(r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		pool, err := d.pool(req.database)
		if err != nil {
			jsonError(w, err.Error(), lintDatabaseStatus(err))
			return
		}
		req.target.Provider = d.provider(req.database)
		opts := upgradecheck.Options{TargetDatabase: req.targetDatabase}
		if req.targetDatabase != "" {
			if opts.TargetDB, err = d.member(req.targetDatabase); err != nil {
				jsonError(w, err.Error(), targetDatabaseStatus(err))
				return
			}
		}
		select {
		case d.slots <- struct{}{}:
			defer func() { <-d.slots }()
		case <-r.Context().Done():
			jsonError(w, "upgrade readiness: request ended while waiting for a slot",
				http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), upgradeReadinessBudget)
		defer cancel()
		rep, err := d.run(ctx, pool, req.target, opts)
		if err == nil && d.trigger != nil {
			publishTrigger(r.Context(), upgradeReadinessPath, req.database,
				func(ctx context.Context) error {
					return d.trigger(ctx, req.database, req.target)
				})
		}
		writeUpgradeReadiness(w, rep, err)
	})
}

func targetDatabaseStatus(err error) int {
	if errors.Is(err, errUnknownTargetDatabase) {
		return http.StatusNotFound
	}
	return http.StatusServiceUnavailable
}

func writeUpgradeReadiness(w http.ResponseWriter, rep upgradecheck.Report, err error) {
	switch {
	case errors.Is(err, upgradecheck.ErrInvalidTarget):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		w.Header().Set("Retry-After", "5")
		jsonError(w, "upgrade readiness not checked: "+err.Error(),
			http.StatusServiceUnavailable)
	default:
		writeJSONStatus(w, http.StatusOK, rep)
	}
}
