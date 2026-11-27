package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func configGetHandler(
	mgr *fleet.DatabaseManager, controller *config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database != "" {
			inst := mgr.GetInstance(database)
			if inst == nil {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			cfg := inst.ConfigSnapshot()
			jsonResponse(w, map[string]any{
				"database":    database,
				"trust_level": cfg.TrustLevel,
				"executor":    cfg.IsExecutorEnabled(),
				"llm":         cfg.IsLLMEnabled(),
				"tags":        cfg.Tags,
			})
			return
		}
		active := controller.Active()
		responseCfg := active.Config
		response := map[string]any{
			"active_generation":  active.Generation,
			"desired_generation": controller.Desired().Generation,
			"last_resync_at":     lastResyncValue(controller),
		}
		response["mode"] = responseCfg.Mode
		response["trust"] = responseCfg.Trust
		response["collector"] = responseCfg.Collector
		response["analyzer"] = responseCfg.Analyzer
		response["safety"] = responseCfg.Safety
		response["llm_enabled"] = responseCfg.LLM.Enabled
		response["advisor"] = responseCfg.Advisor
		response["databases"] = len(responseCfg.Databases)
		jsonResponse(w, response)
	}
}

func configUpdateHandlerWithStore(
	controller *config.ConfigController, cs *store.ConfigStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		expected, ok := configExpectedGeneration(body)
		if !ok {
			jsonError(w, "expected_generation is required",
				http.StatusPreconditionRequired)
			return
		}
		candidate := controller.Desired().Config
		if !applyTrustConfigUpdate(w, body, candidate) {
			return
		}
		result, err := applyConfigUpdateCandidate(
			r, controller, cs, expected, candidate,
		)
		if err != nil {
			// A durable conflict is a 409 that resyncs first (config-05).
			writeControlledApplyError(w, r, controller, err)
			return
		}
		jsonResponse(w, result)
	}
}

func applyConfigUpdateCandidate(
	r *http.Request, controller *config.ConfigController,
	cs *store.ConfigStore, expected uint64, candidate *config.Config,
) (config.ApplyResult, error) {
	if cs == nil {
		return controller.Apply(r.Context(), expected, candidate)
	}
	userID := 0
	if user := UserFromContext(r.Context()); user != nil {
		userID = user.ID
	}
	writes := []store.ConfigOverrideWrite{{
		Key: "trust.level", Value: candidate.Trust.Level,
	}}
	// The stored override never raises trust over an explicit file value.
	applyMaskedWrites(candidate, writes, userID, time.Now())
	return controller.ApplyWithPersistence(
		r.Context(), expected, candidate,
		func(ctx context.Context, snapshot config.ConfigSnapshot) error {
			generation, err := cs.SetOverridesCAS(
				ctx, writes, 0, userID, expected,
			)
			if err == nil && generation != snapshot.Generation {
				return fmt.Errorf("durable generation %d, controller %d",
					generation, snapshot.Generation)
			}
			return err
		},
	)
}

func applyTrustConfigUpdate(
	w http.ResponseWriter, body map[string]any, candidate *config.Config,
) bool {
	if trust, ok := body["trust"]; ok {
		if trustMap, ok := trust.(map[string]any); ok {
			if level, ok := trustMap["level"].(string); ok {
				if !config.IsTrustLevel(level) {
					jsonError(w, "invalid trust level",
						http.StatusBadRequest)
					return false
				}
				candidate.Trust.Level = level
			}
		}
	}
	return true
}

func configExpectedGeneration(body map[string]any) (uint64, bool) {
	raw, ok := body["expected_generation"]
	if !ok {
		return 0, false
	}
	value, ok := raw.(float64)
	if !ok || value < 1 || value != float64(uint64(value)) {
		return 0, false
	}
	return uint64(value), true
}
