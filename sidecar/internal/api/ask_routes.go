package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/ask"
)

// Ask Sage (roadmap phase 3). POST /api/v1/databases/{db}/ask answers a
// question from cited evidence (session auth, every signed-in role); GET
// .../ask/conversations lists the caller's own conversations, GET
// .../ask/conversations/{id} one of them with its answers, and GET
// .../ask/budget today's usage. Operators and admins may have Ask Sage
// open an investigation or queue a finding for approval; viewers only
// read. No route can execute or approve anything.

const (
	askBasePath = "/api/v1/databases/{db}/ask"
	maxAskBody  = 16 << 10
)

type askHandlers struct{ reg *ask.Registry }

func registerAskRoutes(mux *http.ServeMux, reg *ask.Registry) {
	if reg == nil {
		return
	}
	viewer := RequireRole("admin", "operator", "viewer")
	h := askHandlers{reg: reg}
	mux.Handle("POST "+askBasePath, viewer(http.HandlerFunc(h.ask)))
	mux.Handle("GET "+askBasePath+"/conversations", viewer(http.HandlerFunc(h.list)))
	mux.Handle("GET "+askBasePath+"/conversations/{id}", viewer(http.HandlerFunc(h.thread)))
	mux.Handle("GET "+askBasePath+"/budget", viewer(http.HandlerFunc(h.budget)))
}

// askCaller is the session user as an Ask Sage caller.
func askCaller(r *http.Request) ask.Caller {
	u := UserFromContext(r.Context())
	return ask.Caller{Actor: fmt.Sprintf("user:%d", u.ID),
		MayPropose: u.Role == "admin" || u.Role == "operator"}
}

// service resolves {db}; it writes the error response when it fails.
func (h askHandlers) service(w http.ResponseWriter, r *http.Request) (*ask.Service, bool) {
	name := r.PathValue("db")
	if name == "" || name == "all" || validateDatabaseParam(name) != nil {
		sreErrorCode(w, "invalid database name", "invalid_request", http.StatusBadRequest)
		return nil, false
	}
	svc, ok := h.reg.Get(name)
	if !ok {
		sreErrorCode(w, "database not found or Ask Sage is not running for it", "not_found",
			http.StatusNotFound)
		return nil, false
	}
	return svc, true
}

func (h askHandlers) ask(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	var req ask.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		sreErrorCode(w, "body must be {\"question\": string, \"conversation_id\"?: string}",
			"invalid_request", http.StatusBadRequest)
		return
	}
	answer, err := svc.Ask(r.Context(), askCaller(r), req)
	if err != nil {
		askError(w, err)
		return
	}
	jsonResponse(w, answer)
}

func (h askHandlers) list(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	convs, err := svc.Conversations(r.Context(), askCaller(r), limit)
	if err != nil {
		askError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"conversations": convs})
}

func (h askHandlers) thread(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	thread, err := svc.Thread(r.Context(), askCaller(r), r.PathValue("id"))
	if err != nil {
		askError(w, err)
		return
	}
	jsonResponse(w, thread)
}

func (h askHandlers) budget(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	st, err := svc.BudgetStatus(r.Context(), askCaller(r))
	if err != nil {
		askError(w, err)
		return
	}
	jsonResponse(w, st)
}

// askError maps Ask Sage errors to distinguishable responses; store
// failures are logged and answered without their detail.
func askError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ask.ErrInvalid):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
	case errors.Is(err, ask.ErrNotFound):
		sreErrorCode(w, "conversation not found", "not_found", http.StatusNotFound)
	case errors.Is(err, ask.ErrDisabled):
		sreErrorCode(w, "Ask Sage is disabled (ask.enabled: false)", "ask_disabled",
			http.StatusServiceUnavailable)
	default:
		slog.Error("ask sage request failed", "error", err)
		sreErrorCode(w, "Ask Sage is unavailable; try again", "ask_unavailable",
			http.StatusServiceUnavailable)
	}
}
