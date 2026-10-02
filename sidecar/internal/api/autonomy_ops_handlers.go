package api

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/gameday"
	"github.com/pg-sage/sidecar/internal/rollout"
)

// outcome lets an operator flag a family action as harmful or unsafe.
// Recoveries come from the executor's verification, never from a person.
func (h autonomyHandlers) outcome(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Database    string `json:"database"`
		Family      string `json:"family"`
		Class       string `json:"class"`
		ActionLogID int64  `json:"action_log_id"`
		Result      string `json:"result"`
		Detail      string `json:"detail"`
	}
	if !decodeAutonomyBody(w, r, &body) {
		return
	}
	if body.Result != earned.ResultHarmful && body.Result != earned.ResultSafetyViolation {
		sreErrorCode(w, "an operator flags harmful or safety_violation outcomes only",
			"invalid_request", http.StatusBadRequest)
		return
	}
	e, name, ok := h.ledger(w, body.Database)
	if !ok {
		return
	}
	st, err := e.Service.Granted(r.Context(), earned.Family(body.Family),
		earned.ActionClass(body.Class))
	if err == nil {
		err = e.Service.RecordOutcome(r.Context(), earned.Outcome{Database: name,
			ActionLogID: body.ActionLogID, Family: earned.Family(body.Family),
			Class: earned.ActionClass(body.Class), Level: st.Level, Result: body.Result,
			Source: earned.SourceOperator, Actor: autonomyActor(UserFromContext(r.Context())),
			Detail: body.Detail})
	}
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}

// bench ingests an uploaded PGIncidentBench JSON report.
func (h autonomyHandlers) bench(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.ledger(w, r.URL.Query().Get("database"))
	if !ok {
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		sreErrorCode(w, "report unreadable (uploads are limited to 1 MiB; use "+
			"sre.autonomy.bench_results_path for larger reports)", "invalid_request",
			http.StatusBadRequest)
		return
	}
	run, err := e.Service.IngestEvalRun(r.Context(), raw, earned.SourceBench,
		autonomyActor(UserFromContext(r.Context())), "")
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	if !run.Duplicate {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
	jsonResponse(w, map[string]any{"run": run})
}

func (h autonomyHandlers) gameDays(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("database")
	if _, resolved, ok := h.ledger(w, name); ok {
		name = resolved
	} else {
		return
	}
	runner, ok := h.gameDayRunner(name)
	if !ok {
		jsonResponse(w, map[string]any{"enabled": false, "items": []any{}})
		return
	}
	items, err := runner.List(r.Context(), 50)
	if err != nil {
		internalError(w, r, "sre game days", err)
		return
	}
	jsonResponse(w, map[string]any{"enabled": true, "items": items})
}

func (h autonomyHandlers) startGameDay(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ledger(w, r.URL.Query().Get("database"))
	if !ok {
		return
	}
	runner, ok := h.gameDayRunner(name)
	if !ok {
		sreErrorCode(w, "game days are off: set sre.autonomy.game_days.enabled and a "+
			"clone provider", "not_configured", http.StatusConflict)
		return
	}
	if err := runner.Start(r.Context()); err != nil {
		autonomyError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	jsonResponse(w, map[string]any{"started": true, "database": name})
}

func (h autonomyHandlers) rollouts(w http.ResponseWriter, r *http.Request) {
	if h.deps.Canary == nil {
		jsonResponse(w, map[string]any{"enabled": false, "items": []any{}})
		return
	}
	items, err := h.deps.Canary.List(r.Context(), 50)
	if err != nil {
		internalError(w, r, "sre rollouts", err)
		return
	}
	jsonResponse(w, map[string]any{"enabled": true, "items": items})
}

func (h autonomyHandlers) rollout(w http.ResponseWriter, r *http.Request) {
	if h.deps.Canary == nil {
		sreErrorCode(w, "the fleet canary is not configured", "not_found", http.StatusNotFound)
		return
	}
	run, instances, err := h.deps.Canary.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		sreErrorCode(w, "rollout not found", "not_found", http.StatusNotFound)
		return
	}
	if err != nil {
		internalError(w, r, "sre rollout", err)
		return
	}
	jsonResponse(w, map[string]any{"run": run, "instances": instances})
}

func (h autonomyHandlers) startRollout(w http.ResponseWriter, r *http.Request) {
	if h.deps.Canary == nil {
		sreErrorCode(w, "the fleet canary needs fleet mode", "not_configured",
			http.StatusConflict)
		return
	}
	var req rollout.StartRequest
	if !decodeAutonomyBody(w, r, &req) {
		return
	}
	user := UserFromContext(r.Context())
	req.StartedBy = autonomyActor(user)
	if user != nil {
		id := user.ID
		req.ApprovedBy = &id
	}
	req.Targets = trimAll(req.Targets)
	run, err := h.deps.Canary.Start(r.Context(), req)
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	jsonResponse(w, map[string]any{"run": run})
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// gameDayRunner resolves a database's game-day runner; none when game
// days are off.
func (h autonomyHandlers) gameDayRunner(database string) (*gameday.Runner, bool) {
	if h.deps.GameDays == nil {
		return nil, false
	}
	return h.deps.GameDays.Lookup(database)
}
