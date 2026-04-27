package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/store"
)

var (
	ErrFindingNotActionable = errors.New("finding not found or not actionable")
	ErrFindingSQLMismatch   = errors.New("sql does not match finding recommendation")
)

// ExecuteManual runs a specific SQL action outside the normal cycle.
// Used for manual "Take Action" and approved queue items.
// Returns the action_log ID.
func (e *Executor) ExecuteManual(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	approvedBy *int,
) (int64, error) {
	if err := ValidateExecutorSQL(sql); err != nil {
		return 0, fmt.Errorf("SQL validation: %w", err)
	}

	if CheckEmergencyStop(ctx, e.pool) {
		return 0, fmt.Errorf("emergency stop active")
	}
	if err := e.verifyManualFinding(ctx, findingID, sql); err != nil {
		return 0, err
	}

	beforeState := e.snapshotBeforeState(ctx)

	ddlTimeout := e.cfg.Safety.DDLTimeout()
	lockOpt := WithLockTimeout(e.cfg.Safety.LockTimeout())
	if categorizeAction(sql) == "create_index" {
		if err := e.dropInvalidCreateIndexBlockers(
			ctx, sql, ddlTimeout, lockOpt,
		); err != nil {
			return 0, fmt.Errorf(
				"dropping invalid index blocker: %w", err)
		}
		exists, err := e.createIndexCoverageExists(ctx, sql)
		if err != nil {
			return 0, fmt.Errorf("checking existing index coverage: %w", err)
		}
		if exists {
			actionID := e.logManualAction(
				ctx, findingID, sql, rollbackSQL, beforeState, nil,
				approvedBy,
			)
			if actionID > 0 {
				updateActionSuccess(ctx, e.pool, actionID)
			}
			return actionID, nil
		}
	}

	var execErr error
	if categorizeAction(sql) == "analyze" {
		execErr = e.executeManualAnalyze(ctx, findingID, sql)
	} else {
		execErr = e.execManualSQLWithRetry(ctx, sql, ddlTimeout, lockOpt)
	}

	actionID := e.logManualAction(
		ctx, findingID, sql, rollbackSQL,
		beforeState, execErr, approvedBy,
	)
	if execErr != nil {
		return 0, fmt.Errorf("executing SQL: %w", execErr)
	}

	if rollbackSQL != "" && actionID > 0 {
		// Detach the monitor from the caller's context so it
		// survives the HTTP request that approved this action.
		// Without this, the goroutine receives Done() the moment
		// the HTTP handler returns and the rollback-monitor
		// window never elapses. Track under the executor's
		// WaitGroup so Shutdown can wait for it.
		e.monitors.Add(1)
		go func() {
			defer e.monitors.Done()
			MonitorAndRollback(
				context.WithoutCancel(ctx), e.pool, actionID, rollbackSQL,
				e.cfg.Trust.RollbackThresholdPct,
				e.cfg.Trust.RollbackWindowMinutes,
				e.logFn,
				e.shutdownCh,
			)
		}()
	} else if actionID > 0 {
		updateActionSuccess(ctx, e.pool, actionID)
	}

	return actionID, nil
}

// RollbackAction executes the stored rollback SQL for an action log row.
func (e *Executor) RollbackAction(
	ctx context.Context,
	actionID int64,
	reason string,
) error {
	var rollbackSQL *string
	var outcome string
	err := e.pool.QueryRow(ctx,
		`SELECT rollback_sql, outcome
		   FROM sage.action_log WHERE id = $1`,
		actionID,
	).Scan(&rollbackSQL, &outcome)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("action not found")
		}
		return fmt.Errorf("loading action %d: %w", actionID, err)
	}
	if outcome == "rolled_back" {
		return fmt.Errorf("action already rolled back")
	}
	if rollbackSQL == nil || strings.TrimSpace(*rollbackSQL) == "" {
		return fmt.Errorf("action has no rollback SQL")
	}
	if err := ValidateExecutorSQL(*rollbackSQL); err != nil {
		return fmt.Errorf("rollback SQL validation: %w", err)
	}
	if CheckEmergencyStop(ctx, e.pool) {
		return fmt.Errorf("emergency stop active")
	}

	ddlTimeout := e.cfg.Safety.DDLTimeout()
	lockOpt := WithLockTimeout(e.cfg.Safety.LockTimeout())
	var execErr error
	if NeedsConcurrently(*rollbackSQL) || NeedsTopLevel(*rollbackSQL) {
		execErr = ExecConcurrently(ctx, e.pool, *rollbackSQL,
			ddlTimeout, lockOpt)
	} else {
		execErr = ExecInTransaction(ctx, e.pool, *rollbackSQL,
			ddlTimeout, lockOpt)
	}
	if execErr != nil {
		updateActionOutcome(ctx, e.pool, actionID, "rollback_failed",
			"manual rollback failed: "+execErr.Error())
		return fmt.Errorf("executing rollback SQL: %w", execErr)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "manual rollback"
	}
	updateActionOutcome(ctx, e.pool, actionID, "rolled_back", reason)
	return nil
}

func (e *Executor) verifyManualFinding(
	ctx context.Context,
	findingID int,
	sql string,
) error {
	var recommendedSQL *string
	err := e.pool.QueryRow(ctx,
		`SELECT recommended_sql
		   FROM sage.findings
		  WHERE id = $1
		    AND status = 'open'
		    AND acted_on_at IS NULL
		    AND resolved_at IS NULL`,
		findingID,
	).Scan(&recommendedSQL)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFindingNotActionable
		}
		return fmt.Errorf("checking finding %d: %w", findingID, err)
	}
	if recommendedSQL == nil ||
		compactSQL(*recommendedSQL) == "" ||
		!strings.EqualFold(compactSQL(*recommendedSQL), compactSQL(sql)) {
		return ErrFindingSQLMismatch
	}
	return nil
}

func compactSQL(sql string) string {
	return strings.Join(strings.Fields(
		strings.TrimSuffix(strings.TrimSpace(sql), ";"),
	), " ")
}

func (e *Executor) executeManualAnalyze(
	ctx context.Context,
	findingID int,
	sql string,
) error {
	finding, err := e.manualAnalyzeFinding(ctx, findingID, sql)
	if err != nil {
		return err
	}
	return e.executeAnalyze(ctx, finding)
}

func (e *Executor) manualAnalyzeFinding(
	ctx context.Context,
	findingID int,
	sql string,
) (analyzer.Finding, error) {
	var objectIdentifier string
	err := e.pool.QueryRow(ctx,
		`SELECT COALESCE(object_identifier, '')
		   FROM sage.findings
		  WHERE id = $1`,
		findingID,
	).Scan(&objectIdentifier)
	if err != nil {
		return analyzer.Finding{}, fmt.Errorf(
			"loading analyze finding %d: %w", findingID, err)
	}
	return analyzer.Finding{
		ObjectIdentifier: objectIdentifier,
		RecommendedSQL:   sql,
		Detail:           map[string]any{},
	}, nil
}

func (e *Executor) execManualSQLWithRetry(
	ctx context.Context,
	sql string,
	ddlTimeout time.Duration,
	lockOpt DDLOption,
) error {
	isCreateIndex := categorizeAction(sql) == "create_index"
	attempts := 1
	if isCreateIndex {
		attempts = 3
		if err := e.dropInvalidCreateIndexBlockers(
			ctx, sql, ddlTimeout, lockOpt,
		); err != nil {
			return err
		}
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := e.dropInvalidCreateIndexBlockers(
				ctx, sql, ddlTimeout, lockOpt,
			); err != nil {
				return err
			}
			time.Sleep(time.Duration(i) * 500 * time.Millisecond)
		}
		var err error
		if NeedsConcurrently(sql) || NeedsTopLevel(sql) {
			err = ExecConcurrently(ctx, e.pool, sql, ddlTimeout, lockOpt)
		} else {
			err = ExecInTransaction(ctx, e.pool, sql, ddlTimeout, lockOpt)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if !isCreateIndex || !errors.Is(err, ErrLockNotAvailable) {
			return err
		}
	}
	return lastErr
}

// logManualAction records a manually-triggered action.
func (e *Executor) logManualAction(
	ctx context.Context,
	findingID int, sql, rollbackSQL string,
	beforeState map[string]any,
	execErr error, approvedBy *int,
) int64 {
	beforeJSON, _ := json.Marshal(beforeState)
	outcome := actionOutcome(execErr)
	actionType := categorizeAction(sql)

	var actionID int64
	err := e.pool.QueryRow(ctx,
		`INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, rollback_sql,
		  before_state, outcome, approved_by, approved_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7,
		  CASE WHEN $7::int IS NOT NULL THEN now() ELSE NULL END)
		 RETURNING id`,
		actionType, findingID, sql,
		store.NilIfEmpty(rollbackSQL), beforeJSON, outcome,
		approvedBy,
	).Scan(&actionID)
	if err != nil {
		e.logFn("executor",
			"failed to log manual action: %v", err)
		return 0
	}

	if outcome != "failed" {
		e.markFindingActioned(ctx, int64(findingID), actionID)
	}
	return actionID
}

func (e *Executor) dropInvalidCreateIndexBlockers(
	ctx context.Context,
	sql string,
	timeout time.Duration,
	opts ...DDLOption,
) error {
	schemaName, tableName, cols, ok := parseCreateIndexTarget(sql)
	if !ok || len(cols) == 0 {
		return nil
	}
	if schemaName == "" {
		schemaName = "public"
	}
	rows, err := e.pool.Query(ctx,
		`WITH indexed AS (
		    SELECT format('%I.%I', idx_ns.nspname, idx.relname) AS index_name,
		           array_agg(a.attname::text ORDER BY ord.n) AS cols
		      FROM pg_index i
		      JOIN pg_class tbl ON tbl.oid = i.indrelid
		      JOIN pg_namespace tbl_ns ON tbl_ns.oid = tbl.relnamespace
		      JOIN pg_class idx ON idx.oid = i.indexrelid
		      JOIN pg_namespace idx_ns ON idx_ns.oid = idx.relnamespace
		      JOIN unnest(i.indkey) WITH ORDINALITY AS ord(attnum, n)
		           ON ord.attnum > 0
		      JOIN pg_attribute a
		           ON a.attrelid = tbl.oid
		          AND a.attnum = ord.attnum
		     WHERE tbl_ns.nspname = $1
		       AND tbl.relname = $2
		       AND NOT i.indisvalid
		       AND i.indpred IS NULL
		     GROUP BY idx_ns.nspname, idx.relname
		)
		SELECT index_name
		  FROM indexed
		 WHERE cols[1:cardinality($3::text[])] = $3::text[]`,
		schemaName, tableName, cols,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var indexes []string
	for rows.Next() {
		var indexName string
		if err := rows.Scan(&indexName); err != nil {
			return err
		}
		indexes = append(indexes, indexName)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, indexName := range indexes {
		if err := ExecConcurrently(
			ctx, e.pool,
			fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s", indexName),
			timeout, opts...,
		); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) createIndexCoverageExists(
	ctx context.Context,
	sql string,
) (bool, error) {
	schemaName, tableName, cols, ok := parseCreateIndexTarget(sql)
	if !ok || len(cols) == 0 {
		return false, nil
	}
	if schemaName == "" {
		schemaName = "public"
	}

	var one int
	err := e.pool.QueryRow(ctx,
		`WITH indexed AS (
		    SELECT i.indexrelid,
		           array_agg(a.attname::text ORDER BY ord.n) AS cols
		      FROM pg_index i
		      JOIN pg_class tbl ON tbl.oid = i.indrelid
		      JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
		      JOIN unnest(i.indkey) WITH ORDINALITY AS ord(attnum, n)
		           ON ord.attnum > 0
		      JOIN pg_attribute a
		           ON a.attrelid = tbl.oid
		          AND a.attnum = ord.attnum
		     WHERE ns.nspname = $1
		       AND tbl.relname = $2
		       AND i.indisvalid
		       AND i.indisready
		       AND i.indpred IS NULL
		     GROUP BY i.indexrelid
		)
		SELECT 1
		  FROM indexed
		 WHERE cols[1:cardinality($3::text[])] = $3::text[]
		 LIMIT 1`,
		schemaName, tableName, cols,
	).Scan(&one)
	if err == nil {
		return true, nil
	}
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return false, err
}

func parseCreateIndexTarget(sql string) (string, string, []string, bool) {
	compact := strings.Join(strings.Fields(strings.TrimSuffix(
		strings.TrimSpace(sql), ";")), " ")
	upper := strings.ToUpper(compact)
	onIdx := strings.Index(upper, " ON ")
	if onIdx < 0 {
		return "", "", nil, false
	}
	afterOn := strings.TrimSpace(compact[onIdx+4:])
	if strings.HasPrefix(strings.ToUpper(afterOn), "ONLY ") {
		afterOn = strings.TrimSpace(afterOn[5:])
	}
	openParen := strings.Index(afterOn, "(")
	if openParen < 0 {
		return "", "", nil, false
	}
	tableSpec := strings.TrimSpace(afterOn[:openParen])
	if usingIdx := strings.Index(strings.ToUpper(tableSpec), " USING "); usingIdx >= 0 {
		tableSpec = strings.TrimSpace(tableSpec[:usingIdx])
	}
	tableSpec = strings.TrimSpace(tableSpec)
	if tableSpec == "" {
		return "", "", nil, false
	}

	closeParen := matchingCloseParen(afterOn, openParen)
	if closeParen < 0 {
		return "", "", nil, false
	}
	cols := normalizeIndexColumns(afterOn[openParen+1 : closeParen])
	if len(cols) == 0 {
		return "", "", nil, false
	}

	parts := splitQualifiedIdentifier(tableSpec)
	if len(parts) == 1 {
		return "", parts[0], cols, true
	}
	if len(parts) == 2 {
		return parts[0], parts[1], cols, true
	}
	return "", "", nil, false
}

func matchingCloseParen(s string, open int) int {
	depth := 0
	inQuote := false
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return -1
}

func normalizeIndexColumns(s string) []string {
	parts := splitTopLevelCSV(s)
	cols := make([]string, 0, len(parts))
	for _, part := range parts {
		col := strings.TrimSpace(part)
		if col == "" || strings.ContainsAny(col, "()") {
			continue
		}
		fields := strings.Fields(col)
		if len(fields) == 0 {
			continue
		}
		cols = append(cols, unquoteIdentifier(fields[0]))
	}
	return cols
}

func splitTopLevelCSV(s string) []string {
	var parts []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
			}
		case ',':
			if !inQuote && depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func splitQualifiedIdentifier(s string) []string {
	var parts []string
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '.':
			if !inQuote {
				parts = append(parts, unquoteIdentifier(s[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, unquoteIdentifier(s[start:]))
	return parts
}

func unquoteIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}
