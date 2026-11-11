package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Approve now, run in the next window (policy-04): an approval the
// maintenance window alone withholds is scheduled for the next opening;
// POST /actions/{id}/schedule does so explicitly and /unschedule cancels.

// scheduleWindowOnly schedules an approval only the window withholds.
// handled is false when the window is not the only refusal, or no window
// could be planned (an invalid trust window, none within 8 days): the
// caller refuses as before. A row that stopped awaiting approval meanwhile
// (another approval scheduled it) is refused without marking it.
func scheduleWindowOnly(ctx context.Context, as *store.ActionStore,
	exec *executor.Executor, action store.QueuedAction,
	readiness executor.ApprovalReadiness, userID int,
	expectSQL string) (map[string]any, *approvalRefusal, bool) {
	if exec == nil || as == nil || !readiness.PolicyKnown || !readiness.Policy.WindowOnly {
		return nil, nil, false
	}
	scheduled, err := exec.ScheduleReady(ctx, as, action, readiness, userID, expectSQL)
	if errors.Is(err, store.ErrNotSchedulable) {
		return nil, &approvalRefusal{status: http.StatusConflict,
			msg: "not scheduled: " + err.Error()}, true
	}
	if err != nil {
		slog.Warn("approval not scheduled", "queue_id", action.ID, "error", err)
		return nil, nil, false
	}
	return scheduledBody(scheduled), nil, true
}

// scheduledBody is the response for an approval scheduled for its window.
func scheduledBody(s executor.ScheduledApproval) map[string]any {
	body := map[string]any{"ok": true, "queue_id": s.Action.ID, "status": "scheduled",
		"executed": false, "run_after": s.Start.At.UTC(),
		"run_after_label": policy.FormatWindowStart(s.Start.At, s.Start.Location)}
	if !s.Start.ClosesAt.IsZero() {
		body["window_closes_at"] = s.Start.ClosesAt.UTC()
	}
	return body
}

// writeSchedule schedules queue item id for userID's approval.
func writeSchedule(w http.ResponseWriter, r *http.Request, as *store.ActionStore,
	exec *executor.Executor, id, userID int) {
	scheduled, err := exec.ScheduleApproval(r.Context(), as, id, userID, "")
	switch {
	case err == nil:
		jsonResponse(w, scheduledBody(scheduled))
	case errors.Is(err, pgx.ErrNoRows):
		jsonError(w, "action not found", http.StatusNotFound)
	case errors.Is(err, executor.ErrApprovalRunsNow), errors.Is(err, executor.ErrNotWindowOnly),
		errors.Is(err, store.ErrNotSchedulable), errors.Is(err, policy.ErrNoWindowWithinScan):
		jsonError(w, err.Error(), http.StatusConflict)
	default:
		slog.Error("schedule action failed", "action_id", id, "error", err)
		jsonError(w, "failed to schedule action", http.StatusInternalServerError)
	}
}

// writeUnschedule cancels queue item id's schedule.
func writeUnschedule(w http.ResponseWriter, r *http.Request, as *store.ActionStore,
	id, userID int) {
	action, err := as.Unschedule(r.Context(), id, userID)
	switch {
	case err == nil:
		jsonResponse(w, map[string]any{"ok": true, "queue_id": id, "status": action.Status})
	case errors.Is(err, store.ErrNotScheduled):
		jsonError(w, "action is not scheduled", http.StatusConflict)
	default:
		slog.Error("unschedule action failed", "action_id", id, "error", err)
		jsonError(w, "failed to cancel the schedule", http.StatusInternalServerError)
	}
}

// scheduleRoute serves schedule (unschedule=false) or unschedule for the
// standalone store and executor.
func scheduleRoute(as *store.ActionStore, exec *executor.Executor,
	unschedule bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := actionIDFromPath(w, r)
		if !ok {
			return
		}
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if unschedule {
			writeUnschedule(w, r, as, id, user.ID)
			return
		}
		writeSchedule(w, r, as, exec, id, user.ID)
	}
}

// fleetScheduleRoute is scheduleRoute for the database a request names.
func fleetScheduleRoute(mgr *fleet.DatabaseManager, unschedule bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, id, ok := actionInstanceAndID(w, r, mgr)
		if !ok {
			return
		}
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		as := store.NewActionStore(inst.Pool)
		if unschedule {
			writeUnschedule(w, r, as, id, user.ID)
			return
		}
		writeSchedule(w, r, as, inst.Executor, id, user.ID)
	}
}
