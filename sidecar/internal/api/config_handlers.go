package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

func configGlobalGetHandler(
	cs *store.ConfigStore, cfg *config.Config,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merged, err := cs.GetMergedConfig(r.Context(), cfg, 0)
		if err != nil {
			jsonError(w, fmt.Sprintf(
				"loading config: %s", err), 500)
			return
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

		errs := applyConfigOverrides(
			r.Context(), cs, cfg, body, 0, userID)
		if len(errs) > 0 {
			jsonError(w, errs[0],
				http.StatusBadRequest)
			return
		}

		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

func configDBGetHandler(
	cs *store.ConfigStore, cfg *config.Config,
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
			jsonError(w, fmt.Sprintf(
				"loading config: %s", err), 500)
			return
		}

		jsonResponse(w, map[string]any{
			"database_id": dbID,
			"config":      merged,
		})
	}
}

func configDBPutHandler(
	cs *store.ConfigStore, cfg *config.Config,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		dbID, err := strconv.Atoi(idStr)
		if err != nil || dbID < 1 {
			jsonError(w, "invalid database id",
				http.StatusBadRequest)
			return
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

		errs := applyConfigOverrides(
			r.Context(), cs, cfg, body, dbID, userID)
		if len(errs) > 0 {
			jsonError(w, errs[0],
				http.StatusBadRequest)
			return
		}

		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

func configAuditHandler(
	cs *store.ConfigStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseIntDefault(
			r.URL.Query().Get("limit"), 100)
		entries, err := cs.GetAuditLog(r.Context(), limit)
		if err != nil {
			jsonError(w, fmt.Sprintf(
				"loading audit log: %s", err), 500)
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
