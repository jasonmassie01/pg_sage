package api

import (
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
	exec *executor.Executor,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbID, ok := singleModeDatabaseFilter(w, r)
		if !ok {
			return
		}

		actions, err := as.ListPending(r.Context(), dbID)
		if err != nil {
			slog.Error("list pending actions failed", "error", err)
			jsonError(w, "failed to list pending actions", 500)
			return
		}

		result := make([]map[string]any, 0, len(actions))
		for _, a := range actions {
			result = append(result, queuedActionMapWithReadiness(a, exec))
		}
		scheduled, err := scheduledActionMaps(r.Context(), as, "")
		if err != nil {
			slog.Error("list scheduled actions failed", "error", err)
			jsonError(w, "failed to list scheduled actions", 500)
			return
		}
		jsonResponse(w, map[string]any{
			"pending":   result,
			"total":     len(result),
			"scheduled": scheduled,
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
		scheduled := make([]map[string]any, 0)
		failures := make([]map[string]string, 0)
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
			pending, waiting, failure := fleetInstancePending(r.Context(), inst)
			if failure != nil {
				failures = append(failures, failure)
				continue
			}
			allResult = append(allResult, pending...)
			scheduled = append(scheduled, waiting...)
		}
		jsonResponse(w, map[string]any{
			"pending":   allResult,
			"total":     len(allResult),
			"errors":    failures,
			"scheduled": scheduled,
		})
	}
}

// fleetPendingCountHandler returns the aggregate count of pending actions
// across the fleet (or the ?database= one). Every database it could not
// count, including one without a pool, is named in errors.
func fleetPendingCountHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		total := 0
		failures := make([]map[string]string, 0)
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
				// The badge shows "N+" rather than a partial count as exact.
				failures = append(failures, notConnectedFailure(inst.Name))
				continue
			}
			as := store.NewActionStore(inst.Pool)
			count, err := as.PendingCount(r.Context())
			if err != nil {
				failures = append(failures,
					fleetReadFailure(inst.Name, "count pending actions", err))
				continue
			}
			total += count
		}
		jsonResponse(w, map[string]any{"count": total, "errors": failures})
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
		ok := true
		if deps.Fleet != nil {
			result, ok = fleetFindingPendingActions(w, r, deps.Fleet, findingID, result)
		} else if deps.Store != nil {
			result, ok = storeFindingPendingActions(w, r, deps, findingID, result)
		}
		if !ok {
			return
		}
		jsonResponse(w, map[string]any{
			"pending": result,
			"total":   len(result),
		})
	}
}

// fleetFindingPendingActions collects the finding's pending actions from
// each selected database; a database whose read fails is left out.
func fleetFindingPendingActions(
	w http.ResponseWriter, r *http.Request, mgr *fleet.DatabaseManager,
	findingID int, result []map[string]any,
) ([]map[string]any, bool) {
	dbFilter := r.URL.Query().Get("database")
	if err := validateDatabaseParam(dbFilter); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if rejectUnknownDatabase(w, mgr, dbFilter) {
		return nil, false
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
		actions, qerr := as.ListPendingByFinding(
			r.Context(), findingID)
		if qerr != nil {
			continue
		}
		for _, a := range actions {
			m := queuedActionMapWithReadiness(a, inst.Executor)
			m["database_name"] = inst.Name
			result = append(result, m)
		}
	}
	return result, true
}

func storeFindingPendingActions(
	w http.ResponseWriter, r *http.Request, deps *ActionDeps,
	findingID int, result []map[string]any,
) ([]map[string]any, bool) {
	actions, qerr := deps.Store.ListPendingByFinding(
		r.Context(), findingID)
	if qerr != nil {
		slog.Error(
			"finding pending actions failed",
			"finding_id", findingID, "error", qerr)
		jsonError(w,
			"failed to list pending actions", 500)
		return nil, false
	}
	for _, a := range actions {
		result = append(result,
			queuedActionMapWithReadiness(a, deps.Executor))
	}
	return result, true
}
