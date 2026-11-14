package api

import (
	"encoding/json"
	"net/http"

	"github.com/pg-sage/sidecar/internal/ask"
)

// Asynchronous Ask Sage runs (mcp-07b). POST .../ask/runs starts a run
// and answers 202 with {run_id, conversation_id}; GET .../ask/runs/{id}
// is the run's state, progress and, once stored, its answer; POST
// .../ask/runs/{id}/cancel cancels it. Same gates as the question route;
// every route keeps the default 30 s request deadline because the run
// lives on the database runtime's context. Another actor's run is 404.
// With grants on, a database the caller may not read is answered like an
// unknown one on every run route, as on the question route (apiauth-08c).

// askRunRetryAfter is the Retry-After (seconds) of a run over the cap.
const askRunRetryAfter = "5"

func registerAskRunRoutes(mux *http.ServeMux, h askHandlers) {
	scoped := requireReadScope(readDatabase(pathDB))
	route(mux, "POST "+askBasePath+"/runs", gateViewerWrite,
		scoped(http.HandlerFunc(h.startRun)))
	route(mux, "GET "+askBasePath+"/runs/{id}", gateViewer, http.HandlerFunc(h.pollRun))
	route(mux, "POST "+askBasePath+"/runs/{id}/cancel", gateViewerWrite,
		scoped(http.HandlerFunc(h.cancelRun)))
}

func (h askHandlers) startRun(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	var req ask.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		sreErrorCode(w, "body must be {\"question\": string, \"conversation_id\"?: string, "+
			"\"read_only\"?: boolean}", "invalid_request", http.StatusBadRequest)
		return
	}
	started, err := svc.StartRun(r.Context(), askCaller(r), req)
	if err != nil {
		askError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, started)
}

func (h askHandlers) pollRun(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	run, err := svc.PollRun(r.Context(), askCaller(r), r.PathValue("id"))
	if err != nil {
		askError(w, err)
		return
	}
	jsonResponse(w, run)
}

func (h askHandlers) cancelRun(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	run, err := svc.CancelRun(r.Context(), askCaller(r), r.PathValue("id"))
	if err != nil {
		askError(w, err)
		return
	}
	jsonResponse(w, run)
}
