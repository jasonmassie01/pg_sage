package api

import (
	"net/http"
	"sort"

	"github.com/pg-sage/sidecar/internal/earned"
)

// Roadmap 2.4: GET /api/v1/model-lift serves "model lift over
// deterministic, per family": the newest held-out bench measurement of
// the model against the causal graph (Safe Pass, override precision,
// inconclusive-case lift) and whether the model may override the graph's
// root for each family or its roots stay advisory (L1). Bench evidence
// is about the pg_sage build, shared by every database of a deployment;
// ?database= picks the ledger to read (default: the first database).
// Every signed-in role reads it.

const modelLiftPath = "/api/v1/model-lift"

func registerModelLiftRoutes(mux *http.ServeMux, deps *AutonomyDeps) {
	if deps == nil || deps.Ledgers == nil {
		return
	}
	viewer := RequireRole("admin", "operator", "viewer")
	h := autonomyHandlers{deps: deps}
	mux.Handle("GET "+modelLiftPath, viewer(http.HandlerFunc(h.modelLift)))
}

func (h autonomyHandlers) modelLift(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("database")
	switch {
	case len(name) > 200:
		sreErrorCode(w, "database name is too long", "invalid_request",
			http.StatusBadRequest)
		return
	case name == "":
		names := h.deps.Ledgers.Databases()
		sort.Strings(names)
		if len(names) == 0 {
			jsonResponse(w, map[string]any{"database": "", "families": []any{}})
			return
		}
		name = names[0]
	}
	entry, ok := h.deps.Ledgers.Lookup(name)
	if !ok {
		sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
		return
	}
	view, err := entry.Service.ModelLiftView(r.Context())
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, struct {
		Database string `json:"database"`
		earned.ModelLiftView
	}{name, view})
}
