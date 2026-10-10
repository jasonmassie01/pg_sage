package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Agent environments (spec §6.5, §8.3). GET /api/v1/agent-environments/
// {database} (operator) shows a database's label, its identity snapshot
// and what it evaluates to now; PUT (admin) sets the label. Narrowing
// applies at once; widening needs a second admin to repeat it (202 until
// then). A label the live identity cannot verify is refused with 409 and
// the reason. Without a control database the routes answer 503.

const maxAgentEnvBody = 1 << 10

func registerAgentEnvRoutes(mux *http.ServeMux, svc *envbind.Service) {
	operatorUp := RequireRole("admin", "operator")
	admin := RequireRole("admin")
	h := agentEnvHandlers{svc: svc}
	const path = "/api/v1/agent-environments/{database}"
	mux.Handle("GET "+path, operatorUp(http.HandlerFunc(h.get)))
	mux.Handle("PUT "+path, admin(http.HandlerFunc(h.put)))
}

type agentEnvHandlers struct{ svc *envbind.Service }

func (h agentEnvHandlers) database(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.svc == nil {
		sreErrorCode(w, "agent environments need mode: meta or agents.control_database",
			"unavailable", http.StatusServiceUnavailable)
		return "", false
	}
	name := r.PathValue("database")
	if name == "" || name == "all" || validateDatabaseParam(name) != nil {
		sreErrorCode(w, "invalid database name", "invalid_arguments",
			http.StatusUnprocessableEntity)
		return "", false
	}
	return name, true
}

func (h agentEnvHandlers) get(w http.ResponseWriter, r *http.Request) {
	name, ok := h.database(w, r)
	if !ok {
		return
	}
	v, err := h.svc.View(r.Context(), name)
	if err != nil {
		writeAgentEnvError(w, r, err)
		return
	}
	jsonResponse(w, v)
}

// agentEnvPut is a PUT answer: the database's view plus what the change did.
type agentEnvPut struct {
	envbind.View
	Applied      bool        `json:"applied"`
	Pending      bool        `json:"pending"`
	PendingLabel envbind.Env `json:"pending_label,omitempty"`
	RequestedBy  string      `json:"requested_by,omitempty"`
}

func (h agentEnvHandlers) put(w http.ResponseWriter, r *http.Request) {
	name, ok := h.database(w, r)
	if !ok {
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentEnvBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		sreErrorCode(w, "body must be {\"label\": \"branch|dev|stage|prod\"}",
			"invalid_arguments", http.StatusUnprocessableEntity)
		return
	}
	res, v, err := h.svc.SetLabel(r.Context(), name, envbind.Env(body.Label), factActor(r))
	if err != nil {
		writeAgentEnvError(w, r, err)
		return
	}
	out := agentEnvPut{View: v, Applied: res.Applied, Pending: res.Pending}
	if res.Pending {
		out.PendingLabel, out.RequestedBy = res.Record.PendingLabel, res.Record.PendingBy
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	jsonResponse(w, out)
}

func writeAgentEnvError(w http.ResponseWriter, r *http.Request, err error) {
	var refused *envbind.RefusedError
	switch {
	case errors.As(err, &refused):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"verdict": "blocked",
			"reason_code": refused.Reason, "error": refused.Detail, "fix": refused.Fix})
	case errors.Is(err, envbind.ErrUnknownDatabase):
		sreErrorCode(w, "unknown database", "unknown_database", http.StatusNotFound)
	case errors.Is(err, envbind.ErrInvalidEnv), errors.Is(err, envbind.ErrInvalidActor):
		sreErrorCode(w, err.Error(), "invalid_arguments", http.StatusUnprocessableEntity)
	case errors.Is(err, envbind.ErrNoControl), errors.Is(err, envbind.ErrUnbound):
		sreErrorCode(w, err.Error(), "unavailable", http.StatusServiceUnavailable)
	default:
		internalError(w, r, "agent environment", err)
	}
}
