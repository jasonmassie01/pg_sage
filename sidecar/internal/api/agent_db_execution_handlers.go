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
	authorities ...*agentDBLiveAuthority,
) http.HandlerFunc {
	var authority *agentDBLiveAuthority
	if len(authorities) > 0 {
		authority = authorities[0]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		body := readMap(r)
		dep, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		if requestsLiveExecution(body) {
			response, err := executeAuthorizedLiveCreate(
				r.Context(), st, registry, authority, dep, body, r,
			)
			if err != nil {
				agentDBError(w, err)
				return
			}
			jsonResponse(w, response)
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

func requestsLiveExecution(body map[string]any) bool {
	if str(body, "mode") == "live" {
		return true
	}
	for _, key := range []string{
		"plan_hash", "estimate_id", "authorization_id", "idempotency_key",
		"approved", "estimated_cost_usd", "cost_estimate_id", "actor_id",
		"admin_override_reason", "policy",
	} {
		if _, ok := body[key]; ok {
			return true
		}
	}
	return false
}

func livePolicyFromProviderConfig(
	provider string,
	cfg agentdb.ProviderConfig,
) agentdb.LiveProvisionPolicy {
	settings := cfg.Settings
	return agentdb.LiveProvisionPolicy{
		LiveProvisioningEnabled: cfg.Enabled &&
			boolValue(settings, "live_provisioning_enabled"),
		ProviderEnabled:   cfg.Enabled,
		Provider:          provider,
		AllowPublicIP:     boolValue(settings, "allow_public_ip"),
		AllowedRegions:    stringSlice(settings, "allowed_regions"),
		AllowedAccounts:   stringSlice(settings, "allowed_accounts"),
		AllowedProjects:   stringSlice(settings, "allowed_projects"),
		AllowedWorkspaces: stringSlice(settings, "allowed_workspaces"),
		RequireBackupBeforeDrop: boolValue(settings, "require_backup_before_drop") ||
			boolValue(settings, "require_backup_before_destroy"),
		MaxTTLSeconds:       integer(settings, "max_ttl_seconds"),
		MaxEstimatedCostUSD: float(settings, "max_estimated_cost_usd"),
		ExecutionMode: firstString(
			str(settings, "execution_mode"), agentdb.LiveModeApproval,
		),
	}
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
	authorities ...*agentDBLiveAuthority,
) http.HandlerFunc {
	var authority *agentDBLiveAuthority
	if len(authorities) > 0 {
		authority = authorities[0]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		body := readMap(r)
		dep, err := st.Get(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		response, err := executeAuthorizedLiveDestroy(
			r.Context(), st, registry, authority, dep, body, r,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, response)
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
