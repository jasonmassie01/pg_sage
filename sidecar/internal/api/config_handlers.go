package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func configGlobalGetHandler(
	cs *store.ConfigStore, cfg *config.Config,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merged, err := cs.GetMergedConfig(r.Context(), cfg, 0)
		if err != nil {
			slog.Error("loading global config failed", "error", err)
			jsonError(w, "failed to load configuration", 500)
			return
		}
		// execution_mode is per-database in fleet mode
		// but standalone always uses "auto".
		merged["execution_mode"] = map[string]any{
			"value": "auto", "source": "default",
		}

		jsonResponse(w, map[string]any{
			"mode":       cfg.Mode,
			"databases":  len(cfg.Databases),
			"config":     merged,
		})
	}
}

func configGlobalPutHandler(
	cs *store.ConfigStore, cfg *config.Config,
	mgr *fleet.DatabaseManager,
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

		errs := applyConfigOverrides(
			r.Context(), cs, cfg, body, 0, userID)
		if len(errs) > 0 {
			jsonError(w, errs[0],
				http.StatusBadRequest)
			return
		}

		// Sync trust_level to fleet instances for display.
		if _, ok := body["trust.level"]; ok && mgr != nil {
			syncTrustLevelToFleet(mgr, cfg.Trust.Level)
		}

		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

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

		merged, err := cs.GetMergedConfig(
			r.Context(), cfg, dbID)
		if err != nil {
			slog.Error("loading database config failed",
				"database_id", dbID, "error", err)
			jsonError(w, "failed to load configuration", 500)
			return
		}

		// Include execution_mode from sage.databases.
		var execMode string
		qErr := metaPool.QueryRow(r.Context(),
			`SELECT COALESCE(execution_mode, 'manual')
			 FROM sage.databases WHERE id = $1`, dbID,
		).Scan(&execMode)
		if qErr == nil {
			merged["execution_mode"] = map[string]any{
				"value":  execMode,
				"source": "db_override",
			}
		}

		jsonResponse(w, map[string]any{
			"database_id": dbID,
			"config":      merged,
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
		if mode, ok := body["execution_mode"]; ok {
			modeStr := fmt.Sprintf("%v", mode)
			if err := updateDBExecutionMode(
				r.Context(), metaPool, dbID, modeStr,
			); err != nil {
				jsonError(w, err.Error(),
					http.StatusBadRequest)
				return
			}
			// Propagate to running executor.
			if mgr != nil {
				if inst := mgr.GetInstanceByDatabaseID(
					dbID); inst != nil && inst.Executor != nil {
					inst.Executor.SetExecutionMode(modeStr)
				}
			}
			delete(body, "execution_mode")
		}

		if len(body) > 0 {
			errs := applyConfigOverrides(
				r.Context(), cs, cfg, body, dbID, userID)
			if len(errs) > 0 {
				jsonError(w, errs[0],
					http.StatusBadRequest)
				return
			}
			// Propagate trust.level to running executor.
			if tl, ok := body["trust.level"]; ok && mgr != nil {
				if inst := mgr.GetInstanceByDatabaseID(
					dbID); inst != nil {
					level := fmt.Sprintf("%v", tl)
					if inst.Executor != nil {
						if err := inst.Executor.SetTrustLevel(
							level); err != nil {
							jsonError(w, err.Error(),
								http.StatusBadRequest)
							return
						}
					}
					if inst.Status != nil {
						inst.Status.TrustLevel = level
					}
				}
			}
		}

		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

var validExecModes = map[string]bool{
	"auto": true, "approval": true, "manual": true,
}

func updateDBExecutionMode(
	ctx context.Context, pool *pgxpool.Pool,
	dbID int, mode string,
) error {
	if !validExecModes[mode] {
		return fmt.Errorf(
			"execution_mode must be auto, approval, "+
				"or manual, got %q", mode)
	}
	tag, err := pool.Exec(ctx,
		`UPDATE sage.databases
		 SET execution_mode = $1 WHERE id = $2`,
		mode, dbID)
	if err != nil {
		return fmt.Errorf("updating execution mode: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("database %d not found", dbID)
	}
	return nil
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

// syncTrustLevelToFleet updates Status.TrustLevel on all fleet
// instances so the dashboard reflects the global config change.
func syncTrustLevelToFleet(
	mgr *fleet.DatabaseManager, level string,
) {
	for _, inst := range mgr.Instances() {
		if inst.Status != nil {
			inst.Status.TrustLevel = level
		}
	}
}
