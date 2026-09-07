package api

import (
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBIdentitiesHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := st.AgentIdentities(r.Context())
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"identities": rows})
	}
}

func agentDBUpsertIdentityHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		row, err := st.UpsertAgentIdentity(r.Context(), agentdb.AgentIdentityRequest{
			AgentID:     str(m, "agent_id"),
			TenantID:    str(m, "tenant_id"),
			OwnerID:     str(m, "owner_id"),
			DisplayName: str(m, "display_name"),
			Status:      str(m, "status"),
			Metadata:    obj(m, "metadata"),
		})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, row)
	}
}

func agentDBCreatePingTokenHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		token, err := st.CreatePingToken(r.Context(), agentDBID(r),
			agentdb.PingTokenRequest{
				AgentID:        str(m, "agent_id"),
				ExpiresSeconds: integer(m, "expires_seconds"),
			})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, token)
	}
}

func agentDBPingTokensHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokens, err := st.PingTokens(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"tokens": tokens})
	}
}

func agentDBRotatePingTokenHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		token, err := st.RotatePingToken(
			r.Context(),
			agentDBID(r),
			r.PathValue("token_id"),
			agentdb.PingTokenRequest{ExpiresSeconds: integer(m, "expires_seconds")},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, token)
	}
}

func agentDBRevokePingTokenHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		token, err := st.RevokePingToken(
			r.Context(),
			agentDBID(r),
			r.PathValue("token_id"),
			str(m, "reason"),
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, token)
	}
}

func agentDBTokenPingHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			agentDBError(w, agentdb.ErrInvalid)
			return
		}
		m := readMap(r)
		dep, err := st.AgentPing(r.Context(), agentDBID(r), token,
			agentdb.PingRequest{
				Status:  str(m, "status"),
				Metrics: obj(m, "metrics"),
			})
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, dep)
	}
}
