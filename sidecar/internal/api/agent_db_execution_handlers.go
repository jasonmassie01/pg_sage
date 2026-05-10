package api

import (
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBProvisionPreflightHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attempt, err := st.PreflightProvision(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionExecuteHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := readMap(r)
		dep, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		if str(body, "mode") == "live" {
			cfg, err := st.ProviderConfig(r.Context(), dep.Provider)
			if err != nil {
				agentDBError(w, err)
				return
			}
			if !cfg.Enabled {
				agentDBError(w, agentdb.ErrInvalid)
				return
			}
			policy := livePolicyFromProviderConfig(dep.Provider, cfg)
			decision := agentdb.EvaluateLiveProvisionPolicy(
				policy,
				liveRequestFromDeployment(dep, body),
			)
			if !decision.Allowed || decision.RequiresReview {
				agentDBError(w, agentdb.ErrInvalid)
				return
			}
			runner, err := registry.ForProvider(dep.Provider)
			if err != nil {
				agentDBError(w, err)
				return
			}
			if runner.Name() == "dry_run" {
				agentDBError(w, agentdb.ErrRunnerUnavailable)
				return
			}
			attempt, err := st.ExecuteProvisionLive(
				r.Context(),
				agentDBID(r),
				runner,
				agentdb.LiveExecutionRequest{
					Mode:           "live",
					CostEstimateID: str(body, "cost_estimate_id"),
					Policy:         policy,
				},
			)
			if err != nil {
				agentDBError(w, err)
				return
			}
			jsonResponse(w, attempt)
			return
		}
		runner, err := registry.CommandRunnerForProvider(dep.Provider)
		if err != nil {
			agentDBError(w, err)
			return
		}
		attempt, err := st.ExecuteProvision(
			r.Context(),
			agentDBID(r),
			runner,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func livePolicyFromProviderConfig(
	provider string,
	cfg agentdb.ProviderConfig,
) agentdb.LiveProvisionPolicy {
	settings := cfg.Settings
	return agentdb.LiveProvisionPolicy{
		LiveProvisioningEnabled: cfg.Enabled,
		ProviderEnabled:         cfg.Enabled,
		Provider:                provider,
		AllowPublicIP:           boolValue(settings, "allow_public_ip"),
		AllowedRegions:          stringSlice(settings, "allowed_regions"),
		AllowedAccounts:         stringSlice(settings, "allowed_accounts"),
		AllowedProjects:         stringSlice(settings, "allowed_projects"),
		AllowedWorkspaces:       stringSlice(settings, "allowed_workspaces"),
		RequireBackupBeforeDrop: boolValue(settings, "require_backup_before_drop"),
		MaxTTLSeconds:           integer(settings, "max_ttl_seconds"),
		MaxEstimatedCostUSD:     float(settings, "max_estimated_cost_usd"),
	}
}

func liveRequestFromDeployment(
	dep agentdb.Deployment,
	body map[string]any,
) agentdb.LiveProvisionRequest {
	params, _ := dep.Metadata["provider_params"].(map[string]any)
	return agentdb.LiveProvisionRequest{
		Provider:             dep.Provider,
		Region:               firstString(str(params, "region"), str(body, "region")),
		Account:              firstString(str(params, "account"), str(body, "account")),
		Project:              firstString(str(params, "project"), str(body, "project")),
		Workspace:            firstString(str(params, "workspace"), str(body, "workspace")),
		TTLSeconds:           ttlSeconds(dep),
		PublicIP:             boolValue(params, "publicly_accessible") || boolValue(params, "ipv4_enabled"),
		EstimatedCostUSD:     float(body, "estimated_cost_usd"),
		EstimatedCostDoubled: boolValue(body, "estimated_cost_doubled"),
		Approved:             boolValue(body, "approved"),
		AdminOverrideReason:  str(body, "admin_override_reason"),
	}
}

func ttlSeconds(dep agentdb.Deployment) int {
	if dep.LeaseExpiresAt == nil {
		return 0
	}
	return int(time.Until(*dep.LeaseExpiresAt).Seconds())
}

func agentDBProvisionStatusHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dep, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		if dep.LiveMode {
			runner, err := registry.ForProvider(dep.Provider)
			if err != nil {
				agentDBError(w, err)
				return
			}
			attempt, err := st.CheckProvisionStatusLive(
				r.Context(), agentDBID(r), runner,
			)
			if err != nil {
				agentDBError(w, err)
				return
			}
			jsonResponse(w, attempt)
			return
		}
		runner, err := registry.CommandRunnerForProvider(dep.Provider)
		if err != nil {
			agentDBError(w, err)
			return
		}
		attempt, err := st.CheckProvisionStatus(
			r.Context(),
			agentDBID(r),
			runner,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionDestroyDryRunHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dep, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		runner, err := registry.CommandRunnerForProvider(dep.Provider)
		if err != nil {
			agentDBError(w, err)
			return
		}
		attempt, err := st.DestroyProvisionDryRun(
			r.Context(),
			agentDBID(r),
			runner,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionDestroyLiveHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dep, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		cfg, err := st.ProviderConfig(r.Context(), dep.Provider)
		if err != nil {
			agentDBError(w, err)
			return
		}
		decision := agentdb.EvaluateLiveProvisionPolicy(
			livePolicyFromProviderConfig(dep.Provider, cfg),
			liveRequestFromDeployment(dep, nil),
		)
		if !decision.Allowed || decision.RequiresReview {
			agentDBError(w, agentdb.ErrInvalid)
			return
		}
		runner, err := registry.ForProvider(dep.Provider)
		if err != nil {
			agentDBError(w, err)
			return
		}
		attempt, err := st.DestroyProvisionLive(r.Context(), agentDBID(r), runner)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionReconcileHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := st.ReconcileAbandonedDeployments(
			r.Context(),
			time.Now().UTC(),
			registry,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, result)
	}
}

func agentDBBackupCheckHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dep, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		if dep.LiveMode {
			runner, err := registry.ForProvider(dep.Provider)
			if err != nil {
				agentDBError(w, err)
				return
			}
			assurance, err := st.CheckBackupAssuranceLive(
				r.Context(), agentDBID(r), runner,
			)
			if err != nil {
				agentDBError(w, err)
				return
			}
			jsonResponse(w, assurance)
			return
		}
		runner, err := registry.CommandRunnerForProvider(dep.Provider)
		if err != nil {
			agentDBError(w, err)
			return
		}
		assurance, err := st.CheckBackupAssurance(
			r.Context(),
			agentDBID(r),
			runner,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, assurance)
	}
}

func agentDBRestoreDrillDryRunHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dep, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		runner, err := registry.CommandRunnerForProvider(dep.Provider)
		if err != nil {
			agentDBError(w, err)
			return
		}
		attempt, err := st.PlanRestoreDrillDryRun(
			r.Context(),
			agentDBID(r),
			runner,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, attempt)
	}
}

func agentDBProvisionAttemptsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		attempts, err := st.ProvisionAttempts(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"attempts": attempts})
	}
}
