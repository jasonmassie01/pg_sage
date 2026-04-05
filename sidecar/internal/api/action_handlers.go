package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// pendingActionsHandler returns pending approval queue items.
func pendingActionsHandler(
	as *store.ActionStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var dbID *int
		if dbStr := r.URL.Query().Get("database"); dbStr != "" {
			if v, err := strconv.Atoi(dbStr); err == nil {
				dbID = &v
			}
		}

		actions, err := as.ListPending(r.Context(), dbID)
		if err != nil {
			slog.Error("list pending actions failed", "error", err)
			jsonError(w, "failed to list pending actions", 500)
			return
		}

		result := make([]map[string]any, 0, len(actions))
		for _, a := range actions {
			result = append(result, queuedActionMap(a))
		}
		jsonResponse(w, map[string]any{
			"pending": result,
			"total":   len(result),
		})
	}
}

// pendingCountHandler returns the count of pending actions.
func pendingCountHandler(
	as *store.ActionStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		count, err := as.PendingCount(r.Context())
		if err != nil {
			slog.Error("pending count failed", "error", err)
			jsonError(w, "failed to get pending count", 500)
			return
		}
		jsonResponse(w, map[string]any{"count": count})
	}
}

// fleetPendingActionsHandler dynamically resolves pools from
// the fleet manager on each request, surviving database
// delete/re-add cycles.
func fleetPendingActionsHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		allResult := make([]map[string]any, 0)
		for _, pool := range mgr.AllPools() {
			as := store.NewActionStore(pool)
			actions, err := as.ListPending(
				r.Context(), nil)
			if err != nil {
				continue
			}
			for _, a := range actions {
				allResult = append(
					allResult, queuedActionMap(a))
			}
		}
		jsonResponse(w, map[string]any{
			"pending": allResult,
			"total":   len(allResult),
		})
	}
}

// fleetPendingCountHandler returns the aggregate count of
// pending actions across all fleet databases.
func fleetPendingCountHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		total := 0
		for _, pool := range mgr.AllPools() {
			as := store.NewActionStore(pool)
			count, err := as.PendingCount(r.Context())
			if err != nil {
				continue
			}
			total += count
		}
		jsonResponse(w, map[string]any{"count": total})
	}
}

// approveActionHandler approves and executes a queued action.
func approveActionHandler(
	as *store.ActionStore, exec *executor.Executor,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			jsonError(w, "invalid action id", http.StatusBadRequest)
			return
		}

		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required",
				http.StatusUnauthorized)
			return
		}

		action, err := as.Approve(r.Context(), id, user.ID)
		if err != nil {
			slog.Error("approve action failed",
				"action_id", id, "error", err)
			jsonError(w, "failed to approve action",
				http.StatusNotFound)
			return
		}

		approvedBy := user.ID
		actionLogID, execErr := exec.ExecuteManual(
			r.Context(),
			action.FindingID, action.ProposedSQL,
			action.RollbackSQL, &approvedBy,
		)

		if execErr != nil {
			jsonResponse(w, map[string]any{
				"ok":        false,
				"queue_id":  id,
				"error":     execErr.Error(),
				"status":    "approved",
				"executed":  false,
			})
			return
		}

		jsonResponse(w, map[string]any{
			"ok":            true,
			"queue_id":      id,
			"action_log_id": actionLogID,
			"status":        "approved",
			"executed":      true,
		})
	}
}

// rejectActionHandler rejects a queued action with a reason.
func rejectActionHandler(
	as *store.ActionStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			jsonError(w, "invalid action id", http.StatusBadRequest)
			return
		}

		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required",
				http.StatusUnauthorized)
			return
		}

		var body struct {
			Reason string `json:"reason"`
		}
		if decErr := json.NewDecoder(r.Body).Decode(&body); decErr != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		err = as.Reject(r.Context(), id, user.ID, body.Reason)
		if err != nil {
			slog.Error("reject action failed",
				"action_id", id, "error", err)
			jsonError(w, "failed to reject action",
				http.StatusNotFound)
			return
		}

		jsonResponse(w, map[string]any{
			"ok":       true,
			"queue_id": id,
			"status":   "rejected",
		})
	}
}

// manualExecuteHandler triggers manual execution from a finding.
func manualExecuteHandler(
	exec *executor.Executor,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required",
				http.StatusUnauthorized)
			return
		}

		var body struct {
			FindingID int    `json:"finding_id"`
			SQL       string `json:"sql"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body.FindingID == 0 || body.SQL == "" {
			jsonError(w, "finding_id and sql are required",
				http.StatusBadRequest)
			return
		}

		userID := user.ID
		actionLogID, err := exec.ExecuteManual(
			r.Context(), body.FindingID, body.SQL, "",
			&userID,
		)
		if err != nil {
			slog.Error("manual execution failed",
				"finding_id", body.FindingID, "error", err)
			jsonError(w, "execution failed", 500)
			return
		}

		jsonResponse(w, map[string]any{
			"ok":            true,
			"action_log_id": actionLogID,
		})
	}
}

// queuedActionMap converts a QueuedAction to a JSON-friendly map.
func queuedActionMap(a store.QueuedAction) map[string]any {
	m := map[string]any{
		"id":           a.ID,
		"database_id":  a.DatabaseID,
		"finding_id":   a.FindingID,
		"proposed_sql": a.ProposedSQL,
		"rollback_sql": a.RollbackSQL,
		"action_risk":  a.ActionRisk,
		"status":       a.Status,
		"proposed_at":  a.ProposedAt,
		"decided_by":   a.DecidedBy,
		"decided_at":   a.DecidedAt,
		"expires_at":   a.ExpiresAt,
		"reason":       a.Reason,
	}
	return m
}
