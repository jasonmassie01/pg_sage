package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBProviderConfigsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := st.ProviderConfigs(r.Context())
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"provider_configs": rows})
	}
}

func agentDBUpsertProviderConfigHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		provider := r.PathValue("provider")
		if provider == "" {
			provider = str(m, "provider")
		}
		cfg, err := st.UpsertProviderConfig(r.Context(), agentdb.ProviderConfigRequest{
			Provider: provider,
			Enabled:  boolValue(m, "enabled"),
			Settings: obj(m, "settings"),
		})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, cfg)
	}
}
