package main

import (
	"context"
	"errors"
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
	var durable config.DurableSource
	if controlPool != nil {
		// Before any ConfigStore exists: they capture the keyring (CG-01).
		if err := initConfigSecrets(context.Background(), controlPool); err != nil {
			return err
		}
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
		// Replicas sharing this database converge on its generation.
		durable = api.GlobalDurableSource{Store: configStore, Base: loadFileConfigBase}
	}
	// Load checked the file; a dashboard override may have supplied the
	// Slack URL or PagerDuty key, so the full alerting check runs on the
	// effective config (alerting-08).
	if err := cfg.ValidateAlerting(); err != nil {
		return fmt.Errorf("config validation: %w", err)
	}
	if err := validateAlertTargets(cfg); err != nil {
		return fmt.Errorf("config validation: %w", err)
	}
	// After persisted overrides: the notices describe the effective values.
	noticeWindowMeaningChange(cfg.Trust.MaintenanceWindow, logWarn)
	warnFastElevation(cfg, logWarn)
	logTrustMeaning(cfg, logInfo, logWarn)
	noticeLLMSetup(cfg, logInfo)
	var owners []config.ReconfigurationOwner
	if modeRegistersOwner(config.TrustPolicyOwner) {
		owners = append(owners, newTrustPolicyOwner())
	}
	configController = config.NewConfigControllerAtGeneration(
		cfg, generation, nil, owners...,
	)
	// Logs the startup masks now, and each later mask once (config-03).
	configController.SetMaskedOverrideLogger(func(msg string) {
		logWarn("config", "%s", msg)
	})
	if durable != nil {
		configController.SetDurableSource(durable)
	}
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
	// The reloaded file may aim a channel somewhere the policy refuses.
	if err := validateAlertTargets(updated); err != nil {
		return fmt.Errorf("config validation: %w", err)
	}
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
		// Another replica saved first. The resync re-reads this file with
		// the durable overrides, so the reload takes effect (config-05).
		if errors.Is(applyErr, store.ErrConfigGenerationConflict) &&
			resyncConfigFromStore(shutdownCtx) {
			logInfo("config", "hot-reload applied through a resync to generation %d",
				configController.Desired().Generation)
			requestCapabilityRefresh()
			return nil
		}
		return applyErr
	}
	logInfo("config", "hot-reload desired=%d active=%d pending_restart=%d",
		result.DesiredGeneration, result.ActiveGeneration,
		len(result.PendingRestart))
	requestCapabilityRefresh()
	return nil
}

func loadConfigCandidate() (*config.Config, error) {
	candidate, err := config.Load(os.Args[1:])
	if err != nil {
		return nil, err
	}
	applyGeneratedKey(candidate)
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

// applyPersistedGlobalOverrides replays the stored global overrides onto
// candidate (config-02): rows the store would refuse today are skipped with
// a warning naming the key and updated_at, never the value.
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
	api.ReplayStoredOverrides(candidate, overrides, func(msg string) {
		logWarn("config", "%s", msg)
	})
	// Rows valid one by one can still leave an invalid whole. Startup exits
	// and a reload keeps the previous config, rather than every PUT failing.
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("config with the stored global overrides is invalid: %w "+
			"(fix the key in Settings, or DELETE /api/v1/config/global/<key> "+
			"removes a stored override)", err)
	}
	return nil
}
