package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBAuditHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		events, err := st.AuditEvents(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"audit_events": events})
	}
}

func agentDBAuditExportHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := st.AuditJSONL(r.Context(), agentDBID(r))
		if err != nil {
			agentDBError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
