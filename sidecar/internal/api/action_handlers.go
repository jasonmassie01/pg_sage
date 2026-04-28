package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

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
		dbFilter := r.URL.Query().Get("database")
		if err := validateDatabaseParam(dbFilter); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, dbFilter) {
			return
		}
		for _, inst := range sortedFleetInstances(mgr) {
			if dbFilter != "" && dbFilter != "all" &&
				inst.Name != dbFilter {
				continue
			}
			if inst.Pool == nil {
				continue
			}
			as := store.NewActionStore(inst.Pool)
			actions, err := as.ListPending(
				r.Context(), nil)
			if err != nil {
				continue
			}
			for _, a := range actions {
				m := queuedActionMap(a)
				m["database_name"] = inst.Name
				allResult = append(allResult, m)
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
		dbFilter := r.URL.Query().Get("database")
		if err := validateDatabaseParam(dbFilter); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, dbFilter) {
			return
		}
		for _, inst := range sortedFleetInstances(mgr) {
			if dbFilter != "" && dbFilter != "all" &&
				inst.Name != dbFilter {
				continue
			}
			if inst.Pool == nil {
				continue
			}
			as := store.NewActionStore(inst.Pool)
			count, err := as.PendingCount(r.Context())
			if err != nil {
				continue
			}
			total += count
		}
		jsonResponse(w, map[string]any{"count": total})
	}
}

// findingPendingActionsHandler returns any pending queued actions
// linked to the given finding_id. Powers the inline approve/reject
// UI on the Findings page. Fleet mode scans all pools; standalone
// mode uses the single store.
func findingPendingActionsHandler(
	deps *ActionDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		findingID, err := strconv.Atoi(idStr)
		if err != nil || findingID <= 0 {
			jsonError(w,
				"invalid finding id", http.StatusBadRequest)
			return
		}
		result := make([]map[string]any, 0)
		if deps.Fleet != nil {
			dbFilter := r.URL.Query().Get("database")
			if err := validateDatabaseParam(dbFilter); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if rejectUnknownDatabase(w, deps.Fleet, dbFilter) {
				return
			}
			for _, inst := range sortedFleetInstances(deps.Fleet) {
				if dbFilter != "" && dbFilter != "all" &&
					inst.Name != dbFilter {
					continue
				}
				if inst.Pool == nil {
					continue
				}
				as := store.NewActionStore(inst.Pool)
				actions, qerr := as.ListPendingByFinding(
					r.Context(), findingID)
				if qerr != nil {
					continue
				}
				for _, a := range actions {
					m := queuedActionMap(a)
					m["database_name"] = inst.Name
					result = append(result, m)
				}
			}
		} else if deps.Store != nil {
			actions, qerr := deps.Store.ListPendingByFinding(
				r.Context(), findingID)
			if qerr != nil {
				slog.Error(
					"finding pending actions failed",
					"finding_id", findingID, "error", qerr)
				jsonError(w,
					"failed to list pending actions", 500)
				return
			}
			for _, a := range actions {
				result = append(result, queuedActionMap(a))
			}
		}
		jsonResponse(w, map[string]any{
			"pending": result,
			"total":   len(result),
		})
	}
}

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
		as := store.NewActionStore(inst.Pool)
		action, err := as.Approve(r.Context(), id, user.ID)
		if err != nil {
			jsonError(w, "failed to approve action",
				http.StatusNotFound)
			return
		}
		approvedBy := user.ID
		actionLogID, execErr := inst.Executor.ExecuteManual(
			r.Context(), action.FindingID, action.ProposedSQL,
			action.RollbackSQL, &approvedBy,
		)
		if execErr != nil {
			recordApproveExecutionFailure(
				r.Context(), as, id, execErr.Error(),
			)
			jsonResponse(w, map[string]any{
				"ok":       false,
				"queue_id": id,
				"database": inst.Name,
				"error":    execErr.Error(),
				"status":   "failed",
				"executed": false,
			})
			return
		}
		verificationStatus := "verified"
		if action.RollbackSQL != "" {
			verificationStatus = "monitoring"
		}
		recordApproveExecutionSuccess(
			r.Context(), as, id, actionLogID, verificationStatus,
		)
		jsonResponse(w, map[string]any{
			"ok":                  true,
			"queue_id":            id,
			"database":            inst.Name,
			"action_log_id":       actionLogID,
			"status":              "approved",
			"executed":            true,
			"verification_status": verificationStatus,
		})
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
			recordApproveExecutionFailure(
				r.Context(), as, id, execErr.Error(),
			)
			jsonResponse(w, map[string]any{
				"ok":       false,
				"queue_id": id,
				"error":    execErr.Error(),
				"status":   "failed",
				"executed": false,
			})
			return
		}

		verificationStatus := "verified"
		if action.RollbackSQL != "" {
			verificationStatus = "monitoring"
		}
		recordApproveExecutionSuccess(
			r.Context(), as, id, actionLogID, verificationStatus,
		)
		jsonResponse(w, map[string]any{
			"ok":                  true,
			"queue_id":            id,
			"action_log_id":       actionLogID,
			"status":              "approved",
			"executed":            true,
			"verification_status": verificationStatus,
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
		var body struct {
			Reason string `json:"reason"`
		}
		if decErr := json.NewDecoder(r.Body).Decode(&body); decErr != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
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
			if errors.Is(err, executor.ErrFindingNotActionable) {
				jsonError(w, err.Error(), http.StatusNotFound)
				return
			}
			if errors.Is(err, executor.ErrFindingSQLMismatch) {
				jsonError(w, err.Error(), http.StatusBadRequest)
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
		var body struct {
			FindingID   int    `json:"finding_id"`
			SQL         string `json:"sql"`
			RollbackSQL string `json:"rollback_sql"`
			Database    string `json:"database"`
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
		if body.Database == "" || body.Database == "all" {
			jsonError(w, "database is required",
				http.StatusBadRequest)
			return
		}
		if err := validateDatabaseParam(body.Database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
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
			if errors.Is(err, executor.ErrFindingNotActionable) {
				jsonError(w, err.Error(), http.StatusNotFound)
				return
			}
			if errors.Is(err, executor.ErrFindingSQLMismatch) {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, map[string]any{
			"ok":            true,
			"database":      body.Database,
			"action_log_id": actionLogID,
		})
	}
}

func fleetRollbackActionHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, id, ok := actionInstanceAndID(w, r, mgr)
		if !ok {
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if decErr := json.NewDecoder(r.Body).Decode(&body); decErr != nil {
			jsonError(w, "invalid JSON", http.StatusBadRequest)
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
	addLifecycleFields(m, a)
	return m
}

func addLifecycleFields(m map[string]any, a store.QueuedAction) {
	if a.ActionType != "" {
		m["action_type"] = a.ActionType
	}
	if a.IdentityKey != "" {
		m["identity_key"] = a.IdentityKey
	}
	if a.PolicyDecision != "" {
		m["policy_decision"] = a.PolicyDecision
	}
	if len(a.Guardrails) > 0 {
		m["guardrails"] = a.Guardrails
	}
	if a.AttemptCount > 0 {
		m["attempt_count"] = a.AttemptCount
	}
	if a.LastAttemptAt != nil {
		m["last_attempt_at"] = a.LastAttemptAt
	}
	if a.CooldownUntil != nil {
		m["cooldown_until"] = a.CooldownUntil
	}
	if a.FailureFingerprint != "" {
		m["failure_fingerprint"] = a.FailureFingerprint
	}
	if a.LastFailureFingerprint != "" {
		m["last_failure_fingerprint"] = a.LastFailureFingerprint
	}
	if a.VerificationStatus != "" {
		m["verification_status"] = a.VerificationStatus
	}
	if a.ShadowToilMinutes > 0 {
		m["shadow_toil_minutes"] = a.ShadowToilMinutes
	}
	if a.ActionLogID != nil {
		m["action_log_id"] = *a.ActionLogID
	}
}

func actionTimelineMap(row map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{
		"id",
		"case_id",
		"finding_id",
		"status",
		"action_type",
		"risk_tier",
		"proposed_sql",
		"actor",
		"verification_status",
		"rollback_status",
		"created_at",
		"approved_at",
		"executed_at",
		"verified_at",
		"expires_at",
		"lifecycle_state",
		"blocked_reason",
		"attempt_count",
		"cooldown_until",
		"policy_decision",
		"guardrails",
		"shadow_toil_minutes",
	} {
		if v, ok := row[key]; ok {
			out[key] = v
		}
	}
	return out
}
