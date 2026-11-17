package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// GET /api/v1/databases/{db}/upgrade-brief?target_major=&method=
// &target_database= (drill-02a) builds the database's upgrade brief from
// one read-only readiness run and stores it in the database's sage schema
// only when it changed (delta-only), so stored_at is the brief's age.
// target_major defaults to the newest community major. Any signed-in role
// with read access to the database may call it; the upgrade itself stays
// with the operator. It is served by the per-database mux
// (perDatabaseSREMux), whose catch-all keeps "{db}/upgrade-brief" from
// overlapping the fleet-mode "managed/{id}" routes.

const upgradeBriefPath = "/api/v1/databases/{db}/upgrade-brief"

type upgradeBriefDeps struct {
	pool     func(database string) (*pgxpool.Pool, error)
	member   func(name string) (*pgxpool.Pool, error)
	provider func(database string) string
	// options are the database's slot consumers and extended-support rate.
	options func(ctx context.Context, database string,
		pool *pgxpool.Pool) upgradecheck.BriefOptions
	build func(context.Context, *pgxpool.Pool, upgradecheck.Target,
		upgradecheck.BriefOptions) (upgradecheck.Brief, error)
	store func(context.Context, *pgxpool.Pool, upgradecheck.Brief) (upgradecheck.StoredBrief,
		error)
	slots chan struct{}
}

// upgradeBriefView is a brief as served: the stored row it matches, or
// why it could not be stored (the brief is still returned).
type upgradeBriefView struct {
	upgradecheck.Brief
	BriefID    int64     `json:"brief_id"`
	Stored     bool      `json:"stored"`
	StoredAt   time.Time `json:"stored_at"`
	StoreError string    `json:"store_error,omitempty"`
}

func registerUpgradeBriefRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config) {
	up := newUpgradeReadinessDeps(mgr, cfg)
	deps := upgradeBriefDeps{pool: up.pool, member: up.member, provider: up.provider,
		options: func(ctx context.Context, database string,
			pool *pgxpool.Pool) upgradecheck.BriefOptions {
			return upgradeBriefOptions(ctx, cfg, database, pool)
		},
		build: upgradecheck.BuildBrief, store: upgradecheck.StoreBrief,
		slots: make(chan struct{}, upgradeReadinessSlots)}
	route(mux, "GET "+upgradeBriefPath, gateViewer, upgradeBriefHandler(deps))
}

// UpgradeBriefMCP serves MCP upgrade_readiness with brief: true on
// database: the brief from one read-only run (run carries the target
// member), stored when it changed, as the API route does.
func UpgradeBriefMCP(ctx context.Context, cfg *config.Config, database string,
	pool *pgxpool.Pool, target upgradecheck.Target, run upgradecheck.Options) (any, error) {
	opts := upgradeBriefOptions(ctx, cfg, database, pool)
	opts.Options = run
	brief, err := upgradecheck.BuildBrief(ctx, pool, target, opts)
	if err != nil {
		return nil, err
	}
	return storeUpgradeBrief(ctx, upgradecheck.StoreBrief, pool, brief), nil
}

// upgradeBriefOptions are database's confirmed slot consumers (read from
// its facts) and its operator extended-support rate (cost.rates).
func upgradeBriefOptions(ctx context.Context, cfg *config.Config, database string,
	pool *pgxpool.Pool) upgradecheck.BriefOptions {
	var out upgradecheck.BriefOptions
	out.SlotConsumers, out.SlotConsumersErr = slotConsumers(ctx, pool)
	if cfg == nil {
		return out
	}
	config.RLockForHotReload()
	rates := cost.NewConfigRates(cfg.Cost)
	config.RUnlockForHotReload()
	resource := ""
	if rt := cloudtel.Lookup(database); rt != nil {
		resource = rt.Resource()
	}
	if r, ok := rates.Rates(resource, database); ok && r.HasExtendedSupport {
		out.Rate = &upgradecheck.ExtendedSupportRate{PerVCPUHour: r.ExtendedSupportPerVCPUHour,
			VCPUs: r.VCPUs, Currency: r.Currency, AsOf: r.AsOf.Format(time.DateOnly)}
	}
	return out
}

// slotConsumers are the confirmed slot_consumer facts, subject to
// consumer.
func slotConsumers(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	confirmed, err := facts.NewStore(pool).Confirmed(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range confirmed {
		if f.Type == facts.TypeSlotConsumer && f.Kind == facts.KindSlot {
			out[f.Subject] = f.Value["consumer"]
		}
	}
	return out, nil
}

// parseUpgradeBrief reads the path and query; target_major is optional.
func parseUpgradeBrief(r *http.Request) (upgradeReadinessQuery, error) {
	q := r.URL.Query()
	out := upgradeReadinessQuery{database: r.PathValue("db"),
		targetDatabase: q.Get("target_database")}
	if out.database == "" || out.database == "all" ||
		validateDatabaseParam(out.database) != nil {
		return out, fmt.Errorf("invalid database name %q", out.database)
	}
	if err := validateDatabaseParam(out.targetDatabase); err != nil {
		return out, fmt.Errorf("target_database: %w", err)
	}
	out.target = upgradecheck.Target{Method: q.Get("method")}.Normalized()
	check := out.target
	check.Major = upgradecheck.DefaultTargetMajor(0)
	if raw := q.Get("target_major"); raw != "" {
		major, err := strconv.Atoi(raw)
		if err != nil {
			return out, fmt.Errorf("%w: target_major must be an integer major such as "+
				"18", upgradecheck.ErrInvalidTarget)
		}
		out.target.Major, check.Major = major, major
	}
	return out, check.ValidateStatic()
}

func upgradeBriefHandler(d upgradeBriefDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := parseUpgradeBrief(r)
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
		opts := d.options(r.Context(), req.database, pool)
		opts.TargetDatabase = req.targetDatabase
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
			jsonError(w, "upgrade brief: request ended while waiting for a slot",
				http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), upgradeReadinessBudget)
		defer cancel()
		brief, err := d.build(ctx, pool, req.target, opts)
		if err != nil {
			writeUpgradeReadiness(w, upgradecheck.Report{}, err)
			return
		}
		writeJSONStatus(w, http.StatusOK, storeUpgradeBrief(ctx, d.store, pool, brief))
	})
}

// storeUpgradeBrief stores brief when it changed; a failed store still
// serves the brief, with the reason.
func storeUpgradeBrief(ctx context.Context, store func(context.Context, *pgxpool.Pool,
	upgradecheck.Brief) (upgradecheck.StoredBrief, error), pool *pgxpool.Pool,
	brief upgradecheck.Brief) upgradeBriefView {
	view := upgradeBriefView{Brief: brief}
	got, err := store(ctx, pool, brief)
	if err != nil {
		view.StoreError = err.Error()
		return view
	}
	view.BriefID, view.Stored, view.StoredAt = got.ID, got.Stored, got.BuiltAt
	return view
}
