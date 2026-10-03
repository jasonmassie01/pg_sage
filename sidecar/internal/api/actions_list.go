package api

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// actionsSort names the actions ledger's only order (newest event first)
// in its cursors.
const actionsSort = "event_at"

var actionLogKeys = []sortKey{{"executed_at", keyTime}}
var queuedLedgerKeys = []sortKey{{"proposed_at", keyTime}}

// timeWindowSQL renders "AND col >= $n AND col <= $m" for the set bounds.
func timeWindowSQL(col string, from, to time.Time, args []any) (string, []any) {
	sql := ""
	if !from.IsZero() {
		args = append(args, from)
		sql += fmt.Sprintf(" AND %s >= $%d", col, len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		sql += fmt.Sprintf(" AND %s <= $%d", col, len(args))
	}
	return sql, args
}

// buildActionLogPageSQL selects up to limit executed actions after cur,
// newest first, through idx_action_log_time; attempts are counted for
// the page's statements only, inside the same time window. The rows come
// back unordered: mergePage orders them.
func buildActionLogPageSQL(from, to time.Time, cur *listCursor, source string,
	limit int) (string, []any) {
	window, args := timeWindowSQL("executed_at", from, to, nil)
	attemptsWindow, _ := timeWindowSQL("a2.executed_at", from, to, nil)
	rows, args := keysetPageSQL("SELECT * FROM sage.action_log WHERE true"+window,
		actionLogKeys, cur, source, limit, args)
	return actionsWithAttemptsSQL(rows, attemptsWindow, ""), args
}

// buildQueuedLedgerPageSQL selects up to limit not-yet-executed proposals
// after cur, newest first, through idx_action_queue_ledger.
func buildQueuedLedgerPageSQL(from, to time.Time, cur *listCursor, source string,
	limit int) (string, []any) {
	window, args := timeWindowSQL("proposed_at", from, to, nil)
	return keysetPageSQL(queuedActionLedgerSQL+queuedLedgerWhere+window,
		queuedLedgerKeys, cur, source, limit, args)
}

// queuedLedgerWhere matches idx_action_queue_ledger's predicate. A
// proposal without proposed_at never existed in practice (the column
// defaults to now()) and could not be shown anyway.
const queuedLedgerWhere = " WHERE q.status <> 'executed' AND q.proposed_at IS NOT NULL"

// keysetPageSQL appends the keyset predicate, newest-first order and limit
// to a selection whose WHERE clause is already open.
func keysetPageSQL(base string, keys []sortKey, cur *listCursor, source string,
	limit int, args []any) (string, []any) {
	var bound int64
	if cur != nil {
		bound = keysetBoundID(source, cur.Source, cur.ID, true)
	}
	order, after, kargs := keysetSQL(keys, true, cur, bound, len(args)+1)
	args = append(args, kargs...)
	args = append(args, limit)
	return base + after + order + fmt.Sprintf(" LIMIT $%d", len(args)), args
}

// actionLogCountFrom is the executed actions in window for the capped
// total, newest first: the count walks idx_action_log_time (1,001 index
// entries at most) instead of scanning the ledger (perf gate,
// perf-selfexcl).
func actionLogCountFrom(window string) string {
	return "sage.action_log WHERE true" + window + " ORDER BY executed_at DESC"
}

// actionsRequest is one actions list request.
type actionsRequest struct {
	page     listPage
	from, to time.Time
}

// queryActionSources reads one database's executed and queued ledgers
// (each its own keyset source) and their total (up to maxListTotal+1
// each).
func queryActionSources(ctx context.Context, pool *pgxpool.Pool, db string,
	req actionsRequest) (pageResult, error) {
	var res pageResult
	if err := checkCursorKeys(req.page.Cursor, actionLogKeys); err != nil {
		return res, err
	}
	fetch := req.page.Offset + req.page.Limit + 1
	logWindow, logArgs := timeWindowSQL("executed_at", req.from, req.to, nil)
	qWindow, qArgs := timeWindowSQL("proposed_at", req.from, req.to, nil)
	logTotal, err := cappedCount(ctx, pool, actionLogCountFrom(logWindow), logArgs)
	if err != nil {
		return res, fmt.Errorf("count actions: %w", err)
	}
	qTotal, err := cappedCount(ctx, pool,
		"sage.action_queue q"+queuedLedgerWhere+qWindow, qArgs)
	if err != nil {
		return res, fmt.Errorf("count queued action ledger: %w", err)
	}
	res.total = logTotal + qTotal
	sql, args := buildActionLogPageSQL(req.from, req.to, req.page.Cursor, db+"/log", fetch)
	executed, err := queryLedgerRows(ctx, pool, sql, args, db, "/log", scanActionRows)
	if err != nil {
		return res, fmt.Errorf("query actions: %w", err)
	}
	sql, args = buildQueuedLedgerPageSQL(req.from, req.to, req.page.Cursor, db+"/queue",
		fetch)
	queued, err := queryLedgerRows(ctx, pool, sql, args, db, "/queue",
		scanQueuedActionLedgerRows)
	if err != nil {
		return res, fmt.Errorf("query queued action ledger: %w", err)
	}
	res.rows = append(executed, queued...)
	return res, nil
}

// queryLedgerRows runs one ledger page and positions its rows by event
// time, source (database + ledger) and id.
func queryLedgerRows(ctx context.Context, pool *pgxpool.Pool, sql string, args []any,
	db, ledger string, scan func(pgx.Rows) ([]map[string]any, error)) ([]pagedRow, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	maps, err := scan(rows)
	if err != nil {
		return nil, err
	}
	out := make([]pagedRow, 0, len(maps))
	for _, m := range maps {
		id, err := strconv.ParseInt(fmt.Sprint(m["id"]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ledger row id %v: %w", m["id"], err)
		}
		m["database_name"] = db
		out = append(out, pagedRow{row: m, keys: []any{timeFromMap(m, "event_at")},
			source: db + ledger, id: id})
	}
	return out, nil
}

// listActions serves one page of the actions ledger of the selected
// databases: executed actions and queued proposals interleaved by time.
func listActions(ctx context.Context, sources []namedPool, req actionsRequest,
) (map[string]any, error) {
	var all []pagedRow
	sum := 0
	for _, s := range sources {
		res, err := queryActionSources(ctx, s.pool, s.name, req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		all = append(all, res.rows...)
		sum += res.total
	}
	actions, next := mergePage(all, req.page, true, actionsSort, "desc")
	total, capped := capTotal(sum)
	return map[string]any{"total": total, "total_capped": capped,
		"offset": req.page.Offset, "limit": req.page.Limit, "actions": actions,
		"next_cursor": next}, nil
}
