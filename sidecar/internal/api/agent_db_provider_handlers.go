package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBProvidersHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]any{
			"providers": agentdb.ProviderReadinessList(r.Context()),
		})
	}
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
