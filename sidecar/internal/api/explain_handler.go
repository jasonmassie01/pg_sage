package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

func explainHandler(
	mgr *fleet.DatabaseManager, cfg *config.Config,
	llmClient *llm.Client,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cfg.Explain.Enabled {
			jsonError(w, "explain endpoint is disabled",
				http.StatusServiceUnavailable)
			return
		}

		var req explain.ExplainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}

		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		// An unknown database must 404; never fall back to the
		// primary pool (G1-B22).
		if rejectUnknownDatabase(w, mgr, dbName) {
			return
		}
		pool := mgr.PoolForDatabase(dbName)
		if pool == nil {
			jsonError(w, "no database pool available",
				http.StatusServiceUnavailable)
			return
		}

		logFn := func(level, msg string, args ...any) {
			slog.Log(r.Context(), slog.LevelInfo, msg, args...)
		}
		ex := explain.NewWithLLM(pool, &cfg.Explain, llmClient, logFn).
			WithPlanMaxAge(cfg.Tuner.PlanMaxAgeMinutes)
		result, err := ex.Explain(r.Context(), req)
		if err != nil {
			if errors.Is(err, explain.ErrExplainInvalidRequest) {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			internalError(w, r, "explain query", err)
			return
		}
		jsonResponse(w, result)
	}
}
