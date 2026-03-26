package api

import (
	"encoding/json"
	"net/http"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func databasesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, mgr.FleetStatus())
	}
}

func findingsListHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if database == "" {
			database = "all"
		}
		filters := fleet.FindingFilters{
			Status:   q.Get("status"),
			Severity: q.Get("severity"),
			Category: q.Get("category"),
			Sort:     q.Get("sort"),
			Order:    q.Get("order"),
			Limit:    parseIntDefault(q.Get("limit"), 50),
			Offset:   parseIntDefault(q.Get("offset"), 0),
		}
		if filters.Status == "" {
			filters.Status = "open"
		}
		if filters.Sort == "" {
			filters.Sort = "severity"
		}
		if filters.Order == "" {
			filters.Order = "desc"
		}
		if filters.Limit > 200 {
			filters.Limit = 200
		}

		// Return empty list — DB queries added in integration phase
		jsonResponse(w, map[string]any{
			"database": database,
			"filters":  filters,
			"total":    0,
			"offset":   filters.Offset,
			"limit":    filters.Limit,
			"findings": []any{},
		})
	}
}

func findingDetailHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing finding id", http.StatusBadRequest)
			return
		}
		// DB query deferred to integration phase
		jsonError(w, "finding not found", http.StatusNotFound)
	}
}

func suppressHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing finding id", http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]string{
			"id":     id,
			"status": "suppressed",
		})
	}
}

func unsuppressHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing finding id", http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]string{
			"id":     id,
			"status": "open",
		})
	}
}

func actionsListHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if database == "" {
			database = "all"
		}
		limit := parseIntDefault(q.Get("limit"), 50)
		offset := parseIntDefault(q.Get("offset"), 0)
		if limit > 200 {
			limit = 200
		}
		jsonResponse(w, map[string]any{
			"database": database,
			"total":    0,
			"offset":   offset,
			"limit":    limit,
			"actions":  []any{},
		})
	}
}

func actionDetailHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing action id", http.StatusBadRequest)
			return
		}
		jsonError(w, "action not found", http.StatusNotFound)
	}
}

func snapshotLatestHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
		if database == "" {
			jsonError(w, "database parameter required",
				http.StatusBadRequest)
			return
		}
		inst := mgr.GetInstance(database)
		if inst == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]any{
			"database": database,
			"snapshot": nil,
		})
	}
}

func snapshotHistoryHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		metric := q.Get("metric")
		if database == "" {
			jsonError(w, "database parameter required",
				http.StatusBadRequest)
			return
		}
		validMetrics := map[string]bool{
			"cache_hit_ratio": true, "connections": true,
			"tps": true, "dead_tuples": true,
			"database_size": true, "replication_lag": true,
		}
		if metric != "" && !validMetrics[metric] {
			jsonError(w, "invalid metric", http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]any{
			"database": database,
			"metric":   metric,
			"points":   []any{},
		})
	}
}

func configGetHandler(
	mgr *fleet.DatabaseManager, cfg *config.Config,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
		if database != "" {
			inst := mgr.GetInstance(database)
			if inst == nil {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"database":    database,
				"trust_level": inst.Config.TrustLevel,
				"executor":    inst.Config.IsExecutorEnabled(),
				"llm":         inst.Config.IsLLMEnabled(),
				"tags":        inst.Config.Tags,
			})
			return
		}
		jsonResponse(w, map[string]any{
			"mode":        cfg.Mode,
			"trust":       cfg.Trust,
			"collector":   cfg.Collector,
			"analyzer":    cfg.Analyzer,
			"safety":      cfg.Safety,
			"llm_enabled": cfg.LLM.Enabled,
			"advisor":     cfg.Advisor,
			"databases":   len(cfg.Databases),
		})
	}
}

func configUpdateHandler(
	mgr *fleet.DatabaseManager, cfg *config.Config,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		// Apply trust level change if provided
		if trust, ok := body["trust"]; ok {
			if trustMap, ok := trust.(map[string]any); ok {
				if level, ok := trustMap["level"].(string); ok {
					valid := map[string]bool{
						"observation": true, "advisory": true,
						"autonomous": true,
					}
					if !valid[level] {
						jsonError(w, "invalid trust level",
							http.StatusBadRequest)
						return
					}
					cfg.Trust.Level = level
				}
			}
		}
		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

func metricsHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
		status := mgr.FleetStatus()

		if database != "" {
			inst := mgr.GetInstance(database)
			if inst == nil {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"database": database,
				"status":   inst.Status,
			})
			return
		}
		jsonResponse(w, map[string]any{
			"fleet":     status.Summary,
			"databases": status.Databases,
		})
	}
}

func emergencyStopHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
		stopped := mgr.EmergencyStop(database)
		jsonResponse(w, map[string]any{
			"stopped": stopped,
			"status":  "stopped",
		})
	}
}

func resumeHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database := r.URL.Query().Get("database")
		resumed := mgr.Resume(database)
		jsonResponse(w, map[string]any{
			"resumed": resumed,
			"status":  "resumed",
		})
	}
}
