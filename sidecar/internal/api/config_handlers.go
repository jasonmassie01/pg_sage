package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// errInvalidExecMode is returned when the caller supplies a value
// outside the allowed set. Handlers surface this as a 400 so the
// caller can correct the request.
var errInvalidExecMode = errors.New(
	"execution_mode must be auto, approval, or manual")

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
			"last_resync_at":     lastResyncValue(controller),
		}
		merged, err := cs.GetMergedConfig(r.Context(), responseCfg, 0)
		if err != nil {
			slog.Error("loading global config failed", "error", err)
			jsonError(w, "failed to load configuration", 500)
			return
		}
		response["mode"] = responseCfg.Mode
		response["databases"] = len(responseCfg.Databases)
		response["config"] = merged
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
			"read_only":          true,
			"write_guidance":     "edit the YAML file",
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
		executionMode := instance.Config.ExecutionMode
		trustLevel := instance.Config.TrustLevel
		if instance.Executor != nil {
			executionMode = instance.Executor.ExecutionMode()
			trustLevel = instance.Executor.TrustLevel()
		}
		if executionMode == "" {
			executionMode = "approval"
		}
		if trustLevel == "" {
			trustLevel = responseCfg.Trust.Level
		}
		values["execution_mode"] = map[string]any{
			"value": executionMode, "source": "yaml",
		}
		values["trust.level"] = map[string]any{
			"value": trustLevel, "source": "yaml",
		}
		jsonResponse(w, map[string]any{
			"mode":               responseCfg.Mode,
			"databases":          len(responseCfg.Databases),
			"database_id":        dbID,
			"config":             values,
			"desired_generation": desired,
			"active_generation":  active,
			"pending_restart":    pending,
			"read_only":          true,
			"write_guidance":     "edit the YAML file",
		})
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
	writes, validationErrs := validatedConfigWrites(body)
	if len(validationErrs) > 0 {
		jsonError(w, validationErrs[0], http.StatusBadRequest)
		return
	}
	candidate := controller.Desired().Config
	for _, write := range writes {
		hotReload(candidate, write.Key, write.Value)
	}
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
	candidate, err := globalCandidateWithoutOverride(
		r.Context(), cs, base, key,
	)
	if err != nil {
		internalError(w, r, "load desired config revision", err)
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

func configDBGetHandler(
	cs *store.ConfigStore, cfg *config.Config,
	metaPool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		dbID, err := strconv.Atoi(idStr)
		if err != nil || dbID < 1 {
			jsonError(w, "invalid database id",
				http.StatusBadRequest)
			return
		}

		var execMode string
		var trustLevel string
		qErr := metaPool.QueryRow(r.Context(),
			`SELECT COALESCE(execution_mode, 'approval'),
			 COALESCE(trust_level, 'observation')
			 FROM sage.databases WHERE id = $1`, dbID,
		).Scan(&execMode, &trustLevel)
		if qErr != nil {
			if errors.Is(qErr, pgx.ErrNoRows) {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			internalError(w, r, "load database execution mode", qErr)
			return
		}

		merged, err := cs.GetMergedConfig(
			r.Context(), cfg, dbID)
		if err != nil {
			slog.Error("loading database config failed",
				"database_id", dbID, "error", err)
			jsonError(w, "failed to load configuration", 500)
			return
		}
		merged["execution_mode"] = map[string]any{
			"value":  execMode,
			"source": "db_override",
		}
		merged["trust.level"] = map[string]any{
			"value": trustLevel, "source": "database_policy",
		}
		generation, err := cs.GetGeneration(r.Context(), dbID)
		if err != nil {
			internalError(w, r, "load database config generation", err)
			return
		}

		jsonResponse(w, map[string]any{
			"database_id":        dbID,
			"config":             merged,
			"desired_generation": generation,
			"active_generation":  generation,
		})
	}
}

func configDBPutHandler(
	cs *store.ConfigStore, cfg *config.Config,
	metaPool *pgxpool.Pool,
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		dbID, err := strconv.Atoi(idStr)
		if err != nil || dbID < 1 {
			jsonError(w, "invalid database id",
				http.StatusBadRequest)
			return
		}

		// Validate database exists.
		if mgr != nil {
			if mgr.GetInstanceByDatabaseID(dbID) == nil {
				jsonError(w,
					fmt.Sprintf("database %d not found", dbID),
					http.StatusNotFound)
				return
			}
		}

		user := UserFromContext(r.Context())
		userID := 0
		if user != nil {
			userID = user.ID
		}

		var body map[string]any
		if jsonErr := json.NewDecoder(
			r.Body).Decode(&body); jsonErr != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		// Handle execution_mode separately — it lives in
		// sage.databases, not sage.config.
		expected, ok := configExpectedGeneration(body)
		if !ok {
			jsonError(w, "expected_generation is required",
				http.StatusPreconditionRequired)
			return
		}
		delete(body, "expected_generation")
		var executionMode *string
		if raw, ok := body["execution_mode"]; ok {
			mode := fmt.Sprintf("%v", raw)
			if !validExecModes[mode] {
				jsonError(w, fmt.Sprintf("%v: got %q",
					errInvalidExecMode, mode), http.StatusBadRequest)
				return
			}
			executionMode = &mode
			delete(body, "execution_mode")
		}
		writes, validationErrs := validatedConfigWrites(body)
		if len(validationErrs) > 0 {
			jsonError(w, validationErrs[0], http.StatusBadRequest)
			return
		}
		for _, write := range writes {
			if write.Key != "trust.level" {
				jsonError(w, fmt.Sprintf(
					"per-database override %q is not supported; "+
						"only trust.level and execution_mode are active",
					write.Key), http.StatusBadRequest)
				return
			}
		}
		if len(writes) == 0 && executionMode == nil {
			jsonResponse(w, map[string]any{
				"status": "updated", "desired_generation": expected,
				"active_generation": expected,
			})
			return
		}
		var generation uint64
		persistAndApply := func(inst *fleet.DatabaseInstance) error {
			var persistErr error
			generation, persistErr = cs.SetDatabaseOverridesCAS(
				r.Context(), writes, dbID, userID, expected, executionMode,
			)
			if persistErr != nil {
				return persistErr
			}
			if inst == nil {
				return nil
			}
			if executionMode != nil && inst.Executor != nil {
				inst.Executor.SetExecutionMode(*executionMode)
			}
			for _, write := range writes {
				if write.Key == "trust.level" {
					applyDatabaseTrustLevel(inst, write.Value)
				}
			}
			return nil
		}
		if mgr == nil {
			err = persistAndApply(nil)
		} else {
			err = mgr.WithLifecycle(r.Context(), func(*fleet.LifecycleMutation) error {
				inst := mgr.GetInstanceByDatabaseID(dbID)
				if inst == nil {
					return store.ErrConfigDatabaseNotFound
				}
				return persistAndApply(inst)
			})
		}
		if err != nil {
			switch {
			case errors.Is(err, store.ErrConfigGenerationConflict):
				jsonError(w, err.Error(), http.StatusConflict)
			case errors.Is(err, store.ErrConfigDatabaseNotFound):
				jsonError(w, "database not found", http.StatusNotFound)
			default:
				internalError(w, r, "persist database config revision", err)
			}
			return
		}
		applied := make([]string, 0, 2)
		if executionMode != nil {
			applied = append(applied, "execution_mode")
		}
		for _, write := range writes {
			if write.Key == "trust.level" {
				applied = append(applied, write.Key)
			}
		}
		jsonResponse(w, map[string]any{
			"status": "updated", "desired_generation": generation,
			"active_generation": generation, "applied": applied,
			"pending_restart": []string{},
		})
	}
}

func configDBDeleteHandler(
	cs *store.ConfigStore,
	cfg *config.Config,
	mgr *fleet.DatabaseManager,
	controllers ...*config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbID, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || dbID < 1 {
			jsonError(w, "invalid database id",
				http.StatusBadRequest)
			return
		}
		key := r.PathValue("key")
		if key == "" || key == "execution_mode" {
			jsonError(w, "invalid config key",
				http.StatusBadRequest)
			return
		}
		if key == "trust.level" {
			jsonError(w,
				"database trust is a durable policy and cannot be reset to global inheritance",
				http.StatusBadRequest)
			return
		}
		if _, ok := store.AllowedConfigKeysSnapshot()[key]; !ok {
			jsonError(w, "invalid config key",
				http.StatusBadRequest)
			return
		}
		expected, parseErr := strconv.ParseUint(
			r.URL.Query().Get("expected_generation"), 10, 64,
		)
		if parseErr != nil || expected == 0 {
			jsonError(w, "expected_generation is required",
				http.StatusPreconditionRequired)
			return
		}
		userID := 0
		if user := UserFromContext(r.Context()); user != nil {
			userID = user.ID
		}
		generation, err := cs.DeleteOverrideCAS(
			r.Context(), key, dbID, userID, expected,
		)
		if errors.Is(err, store.ErrConfigGenerationConflict) {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			internalError(w, r, "delete database config override", err)
			return
		}
		jsonResponse(w, map[string]any{
			"status": "deleted", "desired_generation": generation,
			"active_generation": generation,
		})
	}
}

var validExecModes = map[string]bool{
	"auto": true, "approval": true, "manual": true,
}

func configAuditHandler(
	cs *store.ConfigStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseIntDefault(
			r.URL.Query().Get("limit"), 100)
		entries, err := cs.GetAuditLog(r.Context(), limit)
		if err != nil {
			slog.Error("loading audit log failed", "error", err)
			jsonError(w, "failed to load audit log", 500)
			return
		}

		result := make([]map[string]any, len(entries))
		for i, e := range entries {
			result[i] = map[string]any{
				"id":          e.ID,
				"key":         e.Key,
				"old_value":   e.OldValue,
				"new_value":   e.NewValue,
				"database_id": e.DatabaseID,
				"changed_by":  e.ChangedBy,
				"changed_at":  e.ChangedAt,
			}
		}
		jsonResponse(w, map[string]any{"audit": result})
	}
}

// applyDatabaseTrustLevel applies an explicit per-database trust policy,
// which a later global trust change does not replace.
func applyDatabaseTrustLevel(inst *fleet.DatabaseInstance, level string) {
	if inst.Executor != nil {
		if err := inst.Executor.SetTrustLevel(level); err != nil {
			slog.Error("apply executor trust level failed",
				"database", inst.Name, "level", level, "error", err)
			return
		}
	}
	inst.SetTrustLevelOverride(true)
	inst.UpdateStatus(func(s *fleet.InstanceStatus) {
		s.TrustLevel = level
	})
}
