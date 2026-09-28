package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

// agentDBProvidersHandler reports readiness from the same runtime registry,
// credential checks and effective policy sources that live execution uses
// (G8-B15, SURF-04).
func agentDBProvidersHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authority == nil {
			jsonResponse(w, map[string]any{
				"providers": agentdb.ProviderReadinessList(r.Context()),
			})
			return
		}
		rows, err := providerReadinessRows(r.Context(), st, registry, authority)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"providers": rows})
	}
}

// providerReadinessRows evaluates each provider with its own global and
// persisted policy layers, the runtime registry and credential checks.
func providerReadinessRows(
	ctx context.Context,
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
) ([]agentdb.ProviderReadiness, error) {
	now := time.Now().UTC()
	rows := agentdb.ProviderReadinessList(ctx)[:1] // local_postgres row
	for _, provider := range agentdb.LiveProviders() {
		opts := agentdb.ProviderReadinessOptions{
			Now: now, Registry: registry,
			RuntimeEnabled:   authority.config.LiveProvisioningEnabled,
			ProviderPolicies: map[string]*agentdb.LivePolicyLayer{},
			RuntimeChecks: map[string]agentdb.ProviderRuntimeCheck{
				provider: runtimeCredentialCheck(registry, provider, now),
			},
		}
		_, global, layer, err := authority.livePolicySources(ctx, st, registry, provider)
		switch {
		case err == nil:
			opts.GlobalPolicy = global
			opts.ProviderPolicies[provider] = layer
		case errors.Is(err, agentdb.ErrNotFound):
			opts.GlobalPolicy = &agentdb.LivePolicyLayer{
				Policy: authority.globalLivePolicy(provider, agentdb.LiveModeApproval),
			}
		default:
			return nil, err
		}
		for _, row := range agentdb.ProviderReadinessList(ctx, opts) {
			if row.Provider == provider {
				rows = append(rows, row)
			}
		}
	}
	return rows, nil
}

func runtimeCredentialCheck(
	registry *agentdb.RunnerRegistry,
	provider string,
	now time.Time,
) agentdb.ProviderRuntimeCheck {
	runner, err := registry.ForProvider(provider)
	if err != nil {
		return agentdb.ProviderRuntimeCheck{}
	}
	reporter, ok := runner.(interface{ CredentialError(time.Time) error })
	if !ok {
		return agentdb.ProviderRuntimeCheck{}
	}
	return agentdb.ProviderRuntimeCheck{CredentialError: reporter.CredentialError(now)}
}

func agentDBListSizeProfilesHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		profiles, err := st.ListSizeProfiles(r.Context())
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"profiles": profiles})
	}
}

func agentDBUpsertSizeProfileHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		profile, err := st.UpsertSizeProfile(r.Context(), agentdb.SizeProfile{
			ProfileID:         str(m, "profile_id"),
			Provider:          str(m, "provider"),
			ProvisioningLevel: str(m, "provisioning_level"),
			Name:              str(m, "name"),
			Description:       str(m, "description"),
			CPU:               float(m, "cpu"),
			MemoryGB:          float(m, "memory_gb"),
			StorageGB:         float(m, "storage_gb"),
			MaxConnections:    integer(m, "max_connections"),
			MonthlyBudgetUSD:  float(m, "monthly_budget_usd"),
			ProviderParams:    obj(m, "provider_params"),
		})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, profile)
	}
}

func agentDBDeleteSizeProfileHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := st.DeleteSizeProfile(r.Context(), r.PathValue("profile_id"))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]bool{"deleted": true})
	}
}
