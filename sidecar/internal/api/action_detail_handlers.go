package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
)

func actionsListHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		page, err := parseListPage(q, actionsSort, "desc")
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		req := actionsRequest{page: page, from: parseTimeParam(q.Get("from")),
			to: parseTimeParam(q.Get("to"))}
		displayName := responseDatabaseName(database)
		if database == "all" && mgr.InstanceCount() == 1 {
			displayName = mgr.ResolveDatabaseName(database)
		}
		sources := poolsForDatabaseSelection(mgr, database)
		if len(sources) == 0 {
			jsonResponse(w, map[string]any{
				"database": displayName, "total": 0, "total_capped": false,
				"offset": page.Offset, "limit": page.Limit,
				"actions": []any{}, "next_cursor": "",
			})
			return
		}
		resp, err := listActions(r.Context(), sources, req)
		if err != nil {
			writeListError(w, r, "actions", err)
			return
		}
		resp["database"] = displayName
		jsonResponse(w, resp)
	}
}

func actionDetailHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			jsonError(w, "missing action id", http.StatusBadRequest)
			return
		}
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, dbName)
		if !ok {
			jsonError(w, "database is required",
				http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			jsonError(w, "action not found", http.StatusNotFound)
			return
		}
		action, err := queryActionByID(
			r.Context(), selected.pool, id,
		)
		if err == nil {
			jsonResponse(w, action)
			return
		}
		jsonError(w, "action not found", http.StatusNotFound)
	}
}

func queryActionByID(
	ctx context.Context, pool *pgxpool.Pool, id string,
) (map[string]any, error) {
	var (
		aID            int64
		executedAt     time.Time
		actionType     string
		findingID      *int64
		sqlExecuted    string
		rollbackSQL    *string
		beforeState    []byte
		afterState     []byte
		outcome        string
		rollbackReason *string
		measuredAt     *time.Time
		outcomeJSON    []byte
	)
	err := pool.QueryRow(ctx, actionDetailSQL, id).Scan(
		&aID, &executedAt, &actionType, &findingID,
		&sqlExecuted, &rollbackSQL, &beforeState,
		&afterState, &outcome, &rollbackReason, &measuredAt, &outcomeJSON,
	)
	if err != nil {
		return nil, err
	}
	a := buildActionMap(
		aID, executedAt, actionType, findingID,
		sqlExecuted, rollbackSQL, beforeState, afterState,
		outcome, rollbackReason, measuredAt,
	)
	annotateOutcome(a, outcomeJSON)
	return a, nil
}

const actionDetailSQL = `/* pg_sage */SELECT id, executed_at,
 action_type, finding_id, sql_executed, rollback_sql,
 before_state, after_state, outcome, rollback_reason,
 measured_at,` + actionOutcomeColumnSQL + ` FROM sage.action_log WHERE id = $1`
