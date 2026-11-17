package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/mcp"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	runtimepkg "github.com/pg-sage/sidecar/internal/migration/runtime"
	"github.com/pg-sage/sidecar/internal/policy"
)

type mcpMigrationFactory func(
	*pgxpool.Pool, policy.Gate, config.CloneProviderConfig,
) (mcp.MigrationRuntime, error)

// rehearsalReceiptMargin is how much longer a rehearsal clone's receipt
// lives than the rehearsal bound, so the reaper never destroys a clone in
// use (migration-07).
const rehearsalReceiptMargin = 15 * time.Minute

// rehearsalReceiptTTL is how long a rehearsal clone's receipt lives: the
// rehearsal bound (never under the default one) plus the margin. A full
// copy's bound includes its restore deadline (lab-05d).
func rehearsalReceiptTTL(cloneConfig config.CloneProviderConfig) time.Duration {
	return max(rehearsalpkg.DefaultMaxDuration, rehearsalMaxDuration(cloneConfig)) +
		rehearsalReceiptMargin
}

// configuredMCPCloneProvider is the clone adapter clone.provider names, or
// nil for none, with no production database: a lakebase_branch adapter
// built here destroys (the reaper) but refuses every create.
func configuredMCPCloneProvider(
	cloneConfig config.CloneProviderConfig,
) (clonepkg.Provider, error) {
	return cloneProviderFor(cloneConfig, nil)
}

// cloneProviderFor is the clone adapter clone.provider names, cloning the
// database monitored connects to (used by lakebase_branch to find and
// guard production's branch), or nil for none.
func cloneProviderFor(cloneConfig config.CloneProviderConfig,
	monitored *pgxpool.Pool) (clonepkg.Provider, error) {
	switch cloneConfig.Provider {
	case "none", "":
		return nil, nil
	case "dle":
		return clonepkg.NewDLEProvider(clonepkg.DLEConfig{
			Endpoint: cloneConfig.DLEEndpoint, Token: cloneConfig.DLEToken,
		})
	case clonepkg.LakebaseAdapter:
		return lakebaseCloneProvider(cloneConfig, monitored)
	case clonepkg.NeonAdapter:
		return neonCloneProvider(cloneConfig, monitored)
	case clonepkg.AuroraAdapter:
		return auroraCloneProvider(cloneConfig, monitored)
	case clonepkg.RDSRestoreAdapter:
		return rdsRestoreCloneProvider(cloneConfig, monitored)
	case clonepkg.CloudSQLCloneAdapter:
		return cloudSQLCloneProvider(cloneConfig, monitored)
	default:
		return nil, errors.New("unsupported clone provider " + cloneConfig.Provider +
			" (this build has none, dle, lakebase_branch, neon_branch, aurora_clone, " +
			"rds_restore or cloudsql_clone)")
	}
}

// cloneReceiptScope is the provider scope receipts are written under: the
// Lakebase or Neon project, the Aurora or RDS region, the Cloud SQL project,
// or the adapter's endpoint without credentials or a trailing slash.
func cloneReceiptScope(cloneConfig config.CloneProviderConfig) string {
	switch cloneConfig.Provider {
	case clonepkg.LakebaseAdapter:
		return "projects/" + cloneConfig.Lakebase.Project
	case clonepkg.NeonAdapter:
		return "projects/" + cloneConfig.Neon.ProjectID
	case clonepkg.AuroraAdapter:
		return "aws/" + cloneConfig.Aurora.Region
	case clonepkg.RDSRestoreAdapter:
		return "aws/" + cloneConfig.RDS.Region
	case clonepkg.CloudSQLCloneAdapter:
		return "gcp/" + cloneConfig.CloudSQL.Project
	}
	parsed, err := url.Parse(cloneConfig.DLEEndpoint)
	if err != nil || parsed.Host == "" {
		return cloneConfig.Provider
	}
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	return strings.TrimRight(parsed.String(), "/")
}

// rehearsalReceiptPool is where rehearsal clone receipts live: agent
// governance's control database, else the target's own sage schema.
func rehearsalReceiptPool(target *pgxpool.Pool) *pgxpool.Pool {
	if control := governanceControlPool(cfg, fleetMgr, globalMetaState); control != nil {
		return control
	}
	return target
}

// receiptingRehearsalProvider wraps adapter so every rehearsal clone is
// receipted in receiptPool; a nil pool creates no clone (no_receipt_store).
// Every receipt names this process as its holder and carries the clone's
// cost label, unpriced (lab-05a).
func receiptingRehearsalProvider(adapter clonepkg.Provider, receiptPool *pgxpool.Pool,
	cloneConfig config.CloneProviderConfig) (clonepkg.Provider, error) {
	return pricedRehearsalProvider(adapter, receiptPool, cloneConfig, nil)
}

// pricedRehearsalProvider is receiptingRehearsalProvider whose cost
// labels are priced by rate (nil: unpriced).
func pricedRehearsalProvider(adapter clonepkg.Provider, receiptPool *pgxpool.Pool,
	cloneConfig config.CloneProviderConfig, rate *clonepkg.Rate) (clonepkg.Provider, error) {
	return clonepkg.NewReceiptingProvider(adapter, clonepkg.NewReceiptStore(receiptPool),
		clonepkg.ReceiptingConfig{
			Adapter: cloneConfig.Provider, Scope: cloneReceiptScope(cloneConfig),
			Deployment: func(ctx context.Context) (string, error) {
				return earned.EnsureDeployment(ctx, receiptPool)
			},
			Purpose: clonepkg.PurposeRehearsal, TTL: rehearsalReceiptTTL(cloneConfig),
			AllowFullCopy: cloneConfig.AllowFullCopy, Rate: rate,
			MaxCreatesPerHour: cloneConfig.MaxCreatesPerHour,
			Holder:            clonepkg.ProcessHolder(),
		})
}

// cloneRateFor is the cost.rates entry pricing database's compute, as a
// clone cost rate; nil (unpriced) when no entry names the database.
func cloneRateFor(cost config.CostConfig, database string) *clonepkg.Rate {
	if database == "" {
		return nil
	}
	for _, r := range cost.Rates {
		if r.Database == database && r.HasCompute() {
			return &clonepkg.Rate{PerHour: r.ComputePerHourValue(), Currency: r.Currency,
				AsOf: r.AsOf, Source: "database " + database}
		}
	}
	return nil
}

func configuredMCPMigrationRuntime(
	pool *pgxpool.Pool, gate policy.Gate, cloneConfig config.CloneProviderConfig,
) (mcp.MigrationRuntime, error) {
	adapter, err := cloneProviderFor(cloneConfig, pool)
	if err != nil || adapter == nil {
		return nil, err
	}
	provider, err := receiptingRehearsalProvider(adapter, rehearsalReceiptPool(pool),
		cloneConfig)
	if err != nil {
		return nil, err
	}
	maxAge := time.Duration(cloneConfig.MaxCloneAgeMinutes) * time.Minute
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	runner := runtimepkg.NewPostgresRehearsalRunner(migrationLockTimeout,
		migrationStatementTimeout).WithProduction(runtimepkg.PoolIdentity(pool)).
		WithWorkload(runtimepkg.PostgresWorkload(pool),
			time.Duration(cloneConfig.RehearsalStatementTimeoutSeconds)*time.Second)
	options := migrationRehearsalOptions(cfg, maxAge)
	options.Provider = cloneConfig.Provider
	// A lakebase_branch rehearsal ends before its branch expires (market-03).
	options.MaxDuration = rehearsalMaxDuration(cloneConfig)
	rehearser := rehearsalpkg.NewOrchestrator(provider, runner, options)
	return runtimepkg.NewOrchestrator(
		migrationPlanner(cfg), recommendOnlyOnCloneFailure{rehearser}, gate,
		runtimepkg.NewPostgresApplier(pool, migrationLockTimeout, migrationStatementTimeout),
		runtimepkg.NewPostgresRecorder(pool),
	), nil
}

// The applier's bounds; the rehearsal judges each step against the same
// statement timeout (migration-06).
const (
	migrationLockTimeout      = planpkg.ContractLockTimeout
	migrationStatementTimeout = rehearsalpkg.DefaultStepStatementTimeout
)

// migrationRehearsalOptions carries rehearsal.plan_flip_pct and
// migration.max_lock_ms into the rehearsal; a nil config (tests) keeps
// the package defaults.
func migrationRehearsalOptions(
	loaded *config.Config, maxAge time.Duration,
) rehearsalpkg.Options {
	options := rehearsalpkg.Options{
		MaxCloneAge: maxAge, RegressionPct: 15,
		MaxDuration:      rehearsalpkg.DefaultMaxDuration,
		StatementTimeout: migrationStatementTimeout,
	}
	if loaded != nil {
		options.PlanFlipPct = loaded.Rehearsal.PlanFlipPct
		options.MaxLock = loaded.Migration.MaxLock()
	}
	return options
}

// migrationPlanner plans with migration.contract_delay_minutes; a nil
// config (tests) keeps the 1h default.
func migrationPlanner(loaded *config.Config) *planpkg.Planner {
	planner := planpkg.NewPlanner()
	if loaded != nil {
		planner.WithContractDelay(loaded.Migration.ContractDelay())
	}
	return planner
}

// rehearsalMaxDuration bounds one rehearsal, clone creation included. A
// lakebase_branch or neon_branch branch expires on the server ttl_minutes
// after its create starts, so the rehearsal ends by then (market-03,
// lab-05b), as it does by an aurora_clone clone's ttl_minutes (lab-05c):
// never later than the clone, and never zero (the orchestrator's
// default), at least a minute. An rds_restore or cloudsql_clone rehearsal
// waits up to clone.restore_deadline_minutes for its full copy, then has
// the default bound (lab-05d, lab-05e).
func rehearsalMaxDuration(cloneConfig config.CloneProviderConfig) time.Duration {
	bound := rehearsalpkg.DefaultMaxDuration
	var minutes int
	switch cloneConfig.Provider {
	case clonepkg.RDSRestoreAdapter, clonepkg.CloudSQLCloneAdapter:
		return restoreDeadline(cloneConfig) + bound
	case clonepkg.LakebaseAdapter:
		minutes = cloneConfig.Lakebase.TTLMinutes
	case clonepkg.NeonAdapter:
		minutes = cloneConfig.Neon.TTLMinutes
	case clonepkg.AuroraAdapter:
		minutes = cloneConfig.Aurora.TTLMinutes
	default:
		return bound
	}
	ttl := time.Duration(minutes) * time.Minute
	if ttl < time.Minute {
		ttl = time.Minute
	}
	return min(bound, ttl)
}

type recommendOnlyOnCloneFailure struct {
	delegate runtimepkg.Rehearser
}

func (wrapper recommendOnlyOnCloneFailure) Rehearse(
	ctx context.Context, planned planpkg.Plan,
) (rehearsalpkg.Result, error) {
	result, err := wrapper.delegate.Rehearse(ctx, planned)
	if err == nil || ctx.Err() != nil {
		return result, err
	}
	result.Verdict = rehearsalpkg.VerdictRecommendOnly
	result.Reason = rehearsalFailureReason(err)
	if result.Reason == rehearsalpkg.ReasonTargetIsProduction {
		logError("migration", "rehearsal refused, recommend only: the clone provider "+
			"returned the production server: %v", err)
	}
	return result, nil
}

// rehearsalFailureReason separates the safety refusals (no receipt store,
// a target that is or may be production) and clone-provider outages from
// genuine step failures on an existing clone, which carry their SQLSTATE
// (G7-B25). Unclassified errors keep the conservative clone reason.
func rehearsalFailureReason(err error) rehearsalpkg.Reason {
	switch {
	case errors.Is(err, clonepkg.ErrNoReceiptStore):
		return rehearsalpkg.ReasonNoReceiptStore
	case errors.Is(err, rehearsalpkg.ErrTargetIsProduction),
		errors.Is(err, clonepkg.ErrCloneIsProduction):
		return rehearsalpkg.ReasonTargetIsProduction
	case errors.Is(err, rehearsalpkg.ErrTargetUnverified):
		return rehearsalpkg.ReasonTargetUnverified
	case !errors.Is(err, rehearsalpkg.ErrStepFailed):
		return rehearsalpkg.Reason("clone_unavailable")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code != "" {
		return rehearsalpkg.Reason("rehearsal_failed:" + pgErr.Code)
	}
	return rehearsalpkg.Reason("rehearsal_failed")
}
