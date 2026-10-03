package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/gameday"
)

// Roadmap 1.1 (2026-10-03): "Run bench locally". The PGIncidentBench
// fault programs run on a disposable clone of the database (the clone
// provider, or the local development database); the report counts as
// bench evidence marked "local run" for the families it covered. Every
// signed-in role reads the status; only an admin starts a run, as for a
// game day or a bench upload.

// localBenchHow says how to make local runs available.
const localBenchHow = "Set clone.provider (dle or snapshot) or " +
	"sre.autonomy.game_days.local_dsn (a disposable database, never a monitored one) " +
	"to run the bench locally."

func (h autonomyHandlers) localBench(database string) (*gameday.LocalBench, bool) {
	return h.deps.LocalBench.Lookup(database)
}

func (h autonomyHandlers) benchRuns(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ledger(w, r.URL.Query().Get("database"))
	if !ok {
		return
	}
	b, ok := h.localBench(name)
	if !ok {
		jsonResponse(w, map[string]any{"enabled": false, "running": false, "last": nil,
			"how": localBenchHow})
		return
	}
	var last any
	if run, ok := b.Last(); ok {
		last = run
	}
	jsonResponse(w, map[string]any{"enabled": true, "provider": b.Provider(),
		"running": b.Running(), "last": last})
}

func (h autonomyHandlers) startBenchRun(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ledger(w, r.URL.Query().Get("database"))
	var body struct {
		Families []string `json:"families"`
	}
	if !ok || !decodeAutonomyBody(w, r, &body) {
		return
	}
	b, ok := h.localBench(name)
	if !ok {
		sreErrorCode(w, "local bench runs are off: "+localBenchHow, "not_configured",
			http.StatusConflict)
		return
	}
	run, err := b.Start(r.Context(), body.Families)
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	jsonResponse(w, map[string]any{"run": run})
}
