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
			if !boolValue(body, "live_enabled") || !boolValue(body, "provider_enabled") {
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
					Policy: agentdb.LiveProvisionPolicy{
						LiveProvisioningEnabled: boolValue(body, "live_enabled"),
						ProviderEnabled:         boolValue(body, "provider_enabled"),
						AllowPublicIP:           boolValue(body, "allow_public_ip"),
						AllowedRegions:          stringSlice(body, "allowed_regions"),
					},
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
