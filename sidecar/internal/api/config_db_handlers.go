package api

import (
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
			"restart_supported":  restartSupported(),
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
		writes, validationErrs := validatedConfigWrites(body, configTargetPolicy(cfg))
		if len(validationErrs) > 0 {
			jsonError(w, validationErrs[0], http.StatusBadRequest)
			return
		}
		for _, write := range writes {
			if !perDatabaseKeys[write.Key] {
				jsonError(w, fmt.Sprintf(
					"per-database override %q is not supported; only trust.level, "+
						"analyzer.disabled_rules and execution_mode are active",
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
				applyDatabaseWrite(inst, write, user)
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
			case errors.Is(err, store.ErrDiscoveredTrust):
				// market-06: adopt the discovered member first.
				jsonError(w, err.Error(), http.StatusConflict)
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
			if perDatabaseKeys[write.Key] {
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
		resetDatabaseOverride(mgr, dbID, key)
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
