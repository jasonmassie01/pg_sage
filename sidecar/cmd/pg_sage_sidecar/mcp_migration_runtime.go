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

// rehearsalReceiptTTL is how long a rehearsal clone's receipt lives: the
// rehearsal bound plus a margin, so the reaper never destroys a clone in
// use (migration-07).
const rehearsalReceiptTTL = rehearsalpkg.DefaultMaxDuration + 15*time.Minute

// configuredMCPCloneProvider is the clone adapter clone.provider names, or
// nil for none. Database Lab is the only adapter in this build.
func configuredMCPCloneProvider(
	cloneConfig config.CloneProviderConfig,
) (clonepkg.Provider, error) {
	switch cloneConfig.Provider {
	case "none", "":
		return nil, nil
	case "dle":
		return clonepkg.NewDLEProvider(clonepkg.DLEConfig{
			Endpoint: cloneConfig.DLEEndpoint, Token: cloneConfig.DLEToken,
		})
	default:
		return nil, errors.New("unsupported clone provider " + cloneConfig.Provider +
			" (this build has none or dle)")
	}
}

// cloneReceiptScope is the provider scope receipts are written under: the
// adapter's endpoint without credentials or a trailing slash.
func cloneReceiptScope(cloneConfig config.CloneProviderConfig) string {
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
func receiptingRehearsalProvider(adapter clonepkg.Provider, receiptPool *pgxpool.Pool,
	cloneConfig config.CloneProviderConfig) (clonepkg.Provider, error) {
	return clonepkg.NewReceiptingProvider(adapter, clonepkg.NewReceiptStore(receiptPool),
		clonepkg.ReceiptingConfig{
			Adapter: cloneConfig.Provider, Scope: cloneReceiptScope(cloneConfig),
			Deployment: func(ctx context.Context) (string, error) {
				return earned.EnsureDeployment(ctx, receiptPool)
			},
			Purpose: clonepkg.PurposeRehearsal, TTL: rehearsalReceiptTTL,
		})
}

func configuredMCPMigrationRuntime(
	pool *pgxpool.Pool, gate policy.Gate, cloneConfig config.CloneProviderConfig,
) (mcp.MigrationRuntime, error) {
	adapter, err := configuredMCPCloneProvider(cloneConfig)
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
	runner := runtimepkg.NewPostgresRehearsalRunner(5*time.Second, 10*time.Minute).
		WithProduction(runtimepkg.PoolIdentity(pool))
	rehearser := rehearsalpkg.NewOrchestrator(provider, runner, rehearsalpkg.Options{
		MaxCloneAge: maxAge, RegressionPct: 15,
		MaxDuration: rehearsalpkg.DefaultMaxDuration,
	})
	return runtimepkg.NewOrchestrator(
		planpkg.NewPlanner(), recommendOnlyOnCloneFailure{rehearser}, gate,
		runtimepkg.NewPostgresApplier(pool, 5*time.Second, 10*time.Minute),
		runtimepkg.NewPostgresRecorder(pool),
	), nil
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
	case errors.Is(err, rehearsalpkg.ErrTargetIsProduction):
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
