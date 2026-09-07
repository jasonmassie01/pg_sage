package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	runtimepkg "github.com/pg-sage/sidecar/internal/migration/runtime"
	"github.com/pg-sage/sidecar/internal/policy"
)

type mcpMigrationFactory func(
	*pgxpool.Pool, policy.Gate, config.CloneProviderConfig,
) (mcp.MigrationRuntime, error)

var mcpSnapshotProviderFactory = func(
	config.CloneProviderConfig,
) (clonepkg.Provider, error) {
	return nil, errors.New("managed snapshot clone adapter is unavailable")
}

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
	case "snapshot":
		return mcpSnapshotProviderFactory(cloneConfig)
	default:
		return nil, errors.New("unsupported MCP clone provider")
	}
}

func configuredMCPMigrationRuntime(
	pool *pgxpool.Pool, gate policy.Gate, cloneConfig config.CloneProviderConfig,
) (mcp.MigrationRuntime, error) {
	provider, err := configuredMCPCloneProvider(cloneConfig)
	if err != nil || provider == nil {
		return nil, err
	}
	maxAge := time.Duration(cloneConfig.MaxCloneAgeMinutes) * time.Minute
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	runner := runtimepkg.NewPostgresRehearsalRunner(5*time.Second, 10*time.Minute)
	rehearser := rehearsalpkg.NewOrchestrator(provider, runner, rehearsalpkg.Options{
		MaxCloneAge: maxAge, RegressionPct: 15,
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
	return rehearsalpkg.Result{
		Verdict: rehearsalpkg.VerdictRecommendOnly,
		Reason:  rehearsalpkg.Reason("clone_unavailable"),
	}, nil
}
