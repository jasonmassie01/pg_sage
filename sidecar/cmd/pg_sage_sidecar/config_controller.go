package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

func initializeConfigController(controlPool *pgxpool.Pool) error {
	if configController != nil {
		return nil
	}
	generation := uint64(1)
	if controlPool != nil {
		configStore := store.NewConfigStore(controlPool)
		var err error
		generation, err = configStore.GetGeneration(context.Background(), 0)
		if err != nil {
			return err
		}
		if err := applyPersistedGlobalOverrides(cfg, controlPool); err != nil {
			return err
		}
		warnInvalidStoredWindows(context.Background(), configStore, logWarn)
	}
	// After persisted overrides: the notice describes the effective window.
	noticeWindowMeaningChange(cfg.Trust.MaintenanceWindow, logWarn)
	noticeLLMSetup(cfg, logInfo)
	configController = config.NewConfigControllerAtGeneration(
		cfg, generation, nil, newTrustPolicyOwner(),
	)
	return nil
}

// ensureConfigController builds the controller for modes whose
// initialization did not (extension). It never replaces a live controller.
func ensureConfigController() error {
	if configController != nil {
		return nil
	}
	return initializeConfigController(configControlPool())
}

// applyWatchedConfig publishes a reloaded config.yaml candidate through the
// controller, persisting the generation when a control database exists.
func applyWatchedConfig(updated *config.Config) error {
	desired := configController.Desired()
	var result config.ApplyResult
	var applyErr error
	controlPool := configControlPool()
	if controlPool == nil {
		result, applyErr = configController.Apply(
			shutdownCtx, desired.Generation, updated,
		)
	} else {
		configStore := store.NewConfigStore(controlPool)
		result, applyErr = configController.ApplyWithPersistence(
			shutdownCtx, desired.Generation, updated,
			func(ctx context.Context, snapshot config.ConfigSnapshot) error {
				generation, err := configStore.SetOverridesCAS(
					ctx, nil, 0, 0, desired.Generation,
				)
				if err == nil && generation != snapshot.Generation {
					return fmt.Errorf("durable generation %d, controller %d",
						generation, snapshot.Generation)
				}
				return err
			},
		)
	}
	if applyErr != nil {
		return applyErr
	}
	logInfo("config", "hot-reload desired=%d active=%d pending_restart=%d",
		result.DesiredGeneration, result.ActiveGeneration,
		len(result.PendingRestart))
	return nil
}

func loadConfigCandidate() (*config.Config, error) {
	candidate, err := config.Load(os.Args[1:])
	if err != nil {
		return nil, err
	}
	controlPool := configControlPool()
	if err := applyPersistedGlobalOverrides(candidate, controlPool); err != nil {
		return nil, err
	}
	return candidate, nil
}

func configControlPool() *pgxpool.Pool {
	if globalMetaState != nil {
		return globalMetaState.Pool
	}
	// Only standalone owns its monitored database's sage schema. YAML fleet
	// has no stable control DB; meta mode persists through globalMetaState.
	if cfg != nil && cfg.IsStandalone() {
		return pool
	}
	return nil
}

func applyPersistedGlobalOverrides(
	candidate *config.Config, controlPool *pgxpool.Pool,
) error {
	if controlPool == nil {
		return nil
	}
	overrides, err := store.NewConfigStore(controlPool).GetOverrides(
		context.Background(), 0,
	)
	if err != nil {
		return fmt.Errorf("load persisted global overrides: %w", err)
	}
	for _, override := range overrides {
		api.ApplyConfigOverrideSnapshot(
			candidate, override.Key, override.Value,
		)
	}
	return nil
}
