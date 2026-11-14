package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func fleetApproveActionHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, id, ok := actionInstanceAndID(w, r, mgr)
		if !ok {
			return
		}
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required",
				http.StatusUnauthorized)
			return
		}
		body, refused := approveAndRun(r.Context(),
			store.NewActionStore(inst.Pool), inst.Executor, id, user.ID)
		if refused != nil {
			refused.write(w, id)
			return
		}
		body["database"] = inst.Name
		jsonResponse(w, body)
	}
}

func fleetRejectActionHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, id, ok := actionInstanceAndID(w, r, mgr)
		if !ok {
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
		body.Reason = strings.TrimSpace(body.Reason)
		if body.Reason == "" {
			jsonError(w, "reason is required", http.StatusBadRequest)
			return
		}
		as := store.NewActionStore(inst.Pool)
		err := as.Reject(r.Context(), id, user.ID, body.Reason)
		if err != nil {
			jsonError(w, "failed to reject action",
				http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]any{
			"ok":       true,
			"queue_id": id,
			"database": inst.Name,
			"status":   "rejected",
		})
	}
}

// approveActionHandler approves and executes a queued action.
func approveActionHandler(
	as *store.ActionStore, exec *executor.Executor,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
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
		body, refused := approveAndRun(r.Context(), as, exec, id, user.ID)
		if refused != nil {
			refused.write(w, id)
			return
		}
		jsonResponse(w, body)
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
		body.Reason = strings.TrimSpace(body.Reason)
		if body.Reason == "" {
			jsonError(w, "reason is required", http.StatusBadRequest)
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

func recordApproveExecutionFailure(
	ctx context.Context,
	as *store.ActionStore,
	queueID int,
	reason string,
) {
	if as == nil {
		return
	}
	_ = as.MarkAttemptFailed(ctx, queueID, reason, time.Hour)
}

func recordApproveExecutionSuccess(
	ctx context.Context,
	as *store.ActionStore,
	queueID int,
	actionLogID int64,
	verificationStatus string,
) {
	if as == nil || actionLogID <= 0 {
		return
	}
	_ = as.MarkExecuted(ctx, queueID, actionLogID, verificationStatus)
}

func readinessOutcomeStatus(
	readiness executor.ApprovalReadiness,
) string {
	switch readiness.Lifecycle.State {
	case store.ActionLifecycleExpired:
		return "expired"
	case store.ActionLifecycleResolvedEphemeral:
		return "resolved_ephemeral"
	default:
		return "blocked"
	}
}

func rollbackActionHandler(
	exec *executor.Executor,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required",
				http.StatusUnauthorized)
			return
		}
		id, ok := actionIDFromPath(w, r)
		if !ok {
			return
		}
		body, ok := decodeRollbackBody(w, r)
		if !ok {
			return
		}
		if err := exec.RollbackAction(
			r.Context(), int64(id), strings.TrimSpace(body.Reason),
		); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]any{
			"ok": true, "action_id": id, "status": "rolled_back",
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
			FindingID   int    `json:"finding_id"`
			SQL         string `json:"sql"`
			RollbackSQL string `json:"rollback_sql"`
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
			r.Context(), body.FindingID, body.SQL, body.RollbackSQL,
			&userID,
		)
		if err != nil {
			slog.Error("manual execution failed",
				"finding_id", body.FindingID, "error", err)
			if status := executor.ManualExecuteStatus(err); status != 500 {
				jsonError(w, err.Error(), status)
				return
			}
			jsonError(w, "execution failed", 500)
			return
		}

		jsonResponse(w, map[string]any{
			"ok":            true,
			"action_log_id": actionLogID,
		})
	}
}

func fleetManualExecuteHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required",
				http.StatusUnauthorized)
			return
		}
		body, ok := decodeFleetManualExecute(w, r)
		if !ok {
			return
		}
		inst := mgr.GetInstance(body.Database)
		if inst == nil || inst.Executor == nil {
			jsonError(w, "database action executor not found",
				http.StatusNotFound)
			return
		}
		userID := user.ID
		actionLogID, err := inst.Executor.ExecuteManual(
			r.Context(), body.FindingID, body.SQL,
			body.RollbackSQL, &userID,
		)
		if err != nil {
			slog.Error("fleet manual execution failed",
				"database", body.Database,
				"finding_id", body.FindingID, "error", err)
			jsonError(w, err.Error(), executor.ManualExecuteStatus(err))
			return
		}
		jsonResponse(w, map[string]any{
			"ok":            true,
			"database":      body.Database,
			"action_log_id": actionLogID,
		})
	}
}

type fleetManualExecuteBody struct {
	FindingID   int    `json:"finding_id"`
	SQL         string `json:"sql"`
	RollbackSQL string `json:"rollback_sql"`
	Database    string `json:"database"`
}

// decodeFleetManualExecute reads and checks the body: a finding, SQL and
// one named database are required.
func decodeFleetManualExecute(
	w http.ResponseWriter, r *http.Request,
) (fleetManualExecuteBody, bool) {
	var body fleetManualExecuteBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return body, false
	}
	if body.FindingID == 0 || body.SQL == "" {
		jsonError(w, "finding_id and sql are required",
			http.StatusBadRequest)
		return body, false
	}
	if body.Database == "" || body.Database == "all" {
		jsonError(w, "database is required",
			http.StatusBadRequest)
		return body, false
	}
	if err := validateDatabaseParam(body.Database); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return body, false
	}
	return body, true
}

func fleetRollbackActionHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := decodeRollbackBody(w, r)
		if !ok {
			return
		}
		inst, id, ok := actionInstanceAndID(w, r, mgr)
		if !ok {
			return
		}
		if err := inst.Executor.RollbackAction(
			r.Context(), int64(id), strings.TrimSpace(body.Reason),
		); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]any{
			"ok": true, "queue_id": id, "database": inst.Name,
			"status": "rolled_back",
		})
	}
}

func actionInstanceAndID(
	w http.ResponseWriter, r *http.Request,
	mgr *fleet.DatabaseManager,
) (*fleet.DatabaseInstance, int, bool) {
	id, ok := actionIDFromPath(w, r)
	if !ok {
		return nil, 0, false
	}
	dbName, ok := readDatabaseParam(w, r)
	if !ok {
		return nil, 0, false
	}
	if dbName == "" || dbName == "all" {
		jsonError(w, "database is required",
			http.StatusBadRequest)
		return nil, 0, false
	}
	inst := mgr.GetInstance(dbName)
	if inst == nil || inst.Pool == nil || inst.Executor == nil {
		jsonError(w, "database action executor not found",
			http.StatusNotFound)
		return nil, 0, false
	}
	return inst, id, true
}

func actionIDFromPath(
	w http.ResponseWriter, r *http.Request,
) (int, bool) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		jsonError(w, "invalid action id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}
