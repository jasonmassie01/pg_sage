package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func configGlobalGetHandler(
	cs *store.ConfigStore, controller *config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		desired := controller.Desired()
		responseCfg := desired.Config
		response := map[string]any{
			"desired_generation": desired.Generation,
			"active_generation":  controller.Active().Generation,
			"pending_restart":    controller.PendingRestart(),
			"pending":            pendingEntries(controller),
			"restart_supported":  restartSupported(),
			"last_resync_at":     lastResyncValue(controller),
		}
		merged, err := cs.GetMergedConfig(r.Context(), responseCfg, 0)
		if err != nil {
			slog.Error("loading global config failed", "error", err)
			jsonError(w, "failed to load configuration", 500)
			return
		}
		markMaskedEntries(merged, responseCfg)
		response["mode"] = responseCfg.Mode
		response["databases"] = len(responseCfg.Databases)
		response["config"] = merged
		response["masked_by_override"] = maskedResponse(responseCfg)
		jsonResponse(w, response)
	}
}

func configReadOnlyGlobalGetHandler(
	controller *config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		responseCfg, desired, active, pending := readOnlyConfigSnapshot(
			controller,
		)
		jsonResponse(w, map[string]any{
			"mode":               responseCfg.Mode,
			"databases":          len(responseCfg.Databases),
			"config":             store.ConfigReadModel(responseCfg),
			"desired_generation": desired,
			"active_generation":  active,
			"pending_restart":    pending,
			"pending":            pendingEntries(controller),
			"restart_supported":  restartSupported(),
			"read_only":          true,
			"write_guidance":     "edit the YAML file",
			"masked_by_override": maskedResponse(responseCfg),
		})
	}
}

func configReadOnlyDBGetHandler(
	mgr *fleet.DatabaseManager, controller *config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbID, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || dbID < 1 {
			jsonError(w, "invalid database id", http.StatusBadRequest)
			return
		}
		if mgr == nil {
			jsonError(w, "fleet manager is unavailable",
				http.StatusServiceUnavailable)
			return
		}
		instance := mgr.GetInstanceByDatabaseID(dbID)
		if instance == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}

		responseCfg, desired, active, pending := readOnlyConfigSnapshot(
			controller,
		)
		values := store.ConfigReadModel(responseCfg)
		addReadOnlyDatabasePolicy(values, instance, responseCfg)
		jsonResponse(w, map[string]any{
			"mode":               responseCfg.Mode,
			"databases":          len(responseCfg.Databases),
			"database_id":        dbID,
			"config":             values,
			"desired_generation": desired,
			"active_generation":  active,
			"pending_restart":    pending,
			"restart_supported":  restartSupported(),
			"read_only":          true,
			"write_guidance":     "edit the YAML file",
		})
	}
}

// addReadOnlyDatabasePolicy reports the database's running execution
// mode and trust level, falling back to its YAML and the global trust.
func addReadOnlyDatabasePolicy(
	values map[string]any, instance *fleet.DatabaseInstance, responseCfg *config.Config,
) {
	executionMode, trustLevel := instanceModeAndTrust(instance, responseCfg.Trust.Level)
	values["execution_mode"] = map[string]any{
		"value": executionMode, "source": store.SourceFileDefault,
	}
	values["trust.level"] = map[string]any{
		"value": trustLevel, "source": store.SourceFileDefault,
	}
}

func readOnlyConfigSnapshot(
	controller *config.ConfigController,
) (*config.Config, uint64, uint64, []string) {
	desired := controller.Desired()
	active := controller.Active()
	return desired.Config, desired.Generation, active.Generation,
		controller.PendingRestart()
}

func configGlobalPutHandler(
	cs *store.ConfigStore, controller *config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		userID := 0
		if user != nil {
			userID = user.ID
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		// execution_mode lives in sage.databases, not
		// sage.config — strip it so it doesn't cause a
		// validation error that blocks other fields.
		delete(body, "execution_mode")
		applyControlledGlobalConfig(w, r, cs, controller, body, userID)
	}
}

func applyControlledGlobalConfig(
	w http.ResponseWriter, r *http.Request,
	cs *store.ConfigStore, controller *config.ConfigController,
	body map[string]any, userID int,
) {
	expected, ok := configExpectedGeneration(body)
	if !ok {
		jsonError(w, "expected_generation is required",
			http.StatusPreconditionRequired)
		return
	}
	delete(body, "expected_generation")
	candidate := controller.Desired().Config
	writes, validationErrs := boundConfigWrites(body, candidate)
	if len(validationErrs) > 0 {
		jsonError(w, validationErrs[0], http.StatusBadRequest)
		return
	}
	for _, write := range writes {
		hotReload(candidate, write.Key, write.Value)
	}
	applyMaskedWrites(candidate, writes, userID, time.Now())
	result, err := controller.ApplyWithPersistence(
		r.Context(), expected, candidate,
		func(ctx context.Context, snapshot config.ConfigSnapshot) error {
			generation, persistErr := cs.SetOverridesCAS(
				ctx, writes, 0, userID, expected,
			)
			if persistErr == nil && generation != snapshot.Generation {
				return fmt.Errorf("durable generation %d, controller %d",
					generation, snapshot.Generation)
			}
			return persistErr
		},
	)
	if err != nil {
		writeControlledApplyError(w, r, controller, err)
		return
	}
	jsonResponse(w, map[string]any{
		"status":             "updated",
		"desired_generation": result.DesiredGeneration,
		"active_generation":  result.ActiveGeneration,
		"applied":            result.Applied,
		"pending_restart":    result.PendingRestart,
		"component_status":   result.ComponentStatus,
		"warnings":           result.Warnings,
	})
}

func writeConfigApplyError(
	w http.ResponseWriter, r *http.Request, err error,
) {
	if errors.Is(err, config.ErrGenerationConflict) ||
		errors.Is(err, store.ErrConfigGenerationConflict) {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	if errors.Is(err, config.ErrInvalidCandidate) {
		// Validation text names the key and the operator's own value.
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	internalError(w, r, "apply config generation", err)
}

func configGlobalDeleteHandler(
	cs *store.ConfigStore, base configBaseSource,
	controller *config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key == "" || key == "execution_mode" {
			jsonError(w, "invalid config key",
				http.StatusBadRequest)
			return
		}
		if _, ok := store.AllowedConfigKeysSnapshot()[key]; !ok {
			jsonError(w, "invalid config key",
				http.StatusBadRequest)
			return
		}
		applyControlledGlobalDelete(w, r, cs, base, controller, key)
	}
}

func applyControlledGlobalDelete(
	w http.ResponseWriter, r *http.Request,
	cs *store.ConfigStore, base configBaseSource,
	controller *config.ConfigController, key string,
) {
	expected, err := strconv.ParseUint(
		r.URL.Query().Get("expected_generation"), 10, 64,
	)
	if err != nil || expected == 0 {
		jsonError(w, "expected_generation is required",
			http.StatusPreconditionRequired)
		return
	}
	candidate, ok := controlledDeleteCandidate(
		w, r, cs, base, key, controller.Desired().Config)
	if !ok {
		return
	}
	result, err := controller.ApplyWithPersistence(
		r.Context(), expected, candidate,
		func(ctx context.Context, snapshot config.ConfigSnapshot) error {
			userID := 0
			if user := UserFromContext(r.Context()); user != nil {
				userID = user.ID
			}
			generation, persistErr := cs.DeleteOverrideCAS(
				ctx, key, 0, userID, expected,
			)
			if persistErr == nil && generation != snapshot.Generation {
				return fmt.Errorf("durable generation %d, controller %d",
					generation, snapshot.Generation)
			}
			return persistErr
		},
	)
	if err != nil {
		writeControlledApplyError(w, r, controller, err)
		return
	}
	jsonResponse(w, map[string]any{
		"status":             "deleted",
		"desired_generation": result.DesiredGeneration,
		"active_generation":  result.ActiveGeneration,
		"pending_restart":    result.PendingRestart,
		"warnings":           result.Warnings,
	})
}

// globalCandidateWithoutOverride rebuilds the desired config from the
// current file base plus every override except omittedKey (G5-B03).
func globalCandidateWithoutOverride(
	ctx context.Context, cs *store.ConfigStore,
	base configBaseSource, omittedKey string,
) (*config.Config, error) {
	candidate, err := base()
	if err != nil {
		return nil, err
	}
	overrides, err := cs.GetOverrides(ctx, 0)
	if err != nil {
		return nil, err
	}
	kept := make([]store.ConfigOverride, 0, len(overrides))
	for _, override := range overrides {
		if override.Key != omittedKey {
			kept = append(kept, override)
		}
	}
	ReplayStoredOverrides(candidate, kept, warnStoredOverride)
	return candidate, nil
}

func warnStoredOverride(msg string) { slog.Warn(msg) }
