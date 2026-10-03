package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ledger rows come from two id spaces: sage.action_log (executed) and
// sage.action_queue (queued proposals). record_kind and ledger_key keep
// them apart so a queue id can never be treated as an action_log id
// (G9-B01 / G6-B05 / G9-B14).
const (
	recordKindExecuted = "executed"
	recordKindQueued   = "queued"
)

// actionsSelectSQLPrefix selects executed actions with all-time attempts.
var actionsSelectSQLPrefix = actionsSelectSQL("")

// attemptsSQL counts the executions of a row's SQL (inside the list's
// time window when window is set) through idx_action_log_sql_md5, for the
// rows returned only. It replaced COUNT(*) OVER (PARTITION BY
// sql_executed), which sorted all of action_log on every page.
func attemptsSQL(window string) string {
	return `(SELECT count(*) FROM sage.action_log a2
   WHERE md5(a2.sql_executed) = md5(action_log.sql_executed)
     AND a2.sql_executed = action_log.sql_executed` + window + `)`
}

func actionsSelectSQL(window string) string {
	return `/* pg_sage */SELECT id, executed_at,
 action_type, finding_id, sql_executed, rollback_sql,
 before_state, after_state, outcome, rollback_reason,
 measured_at,
 ` + attemptsSQL(window) + ` AS attempts,` + actionsVerificationSQL
}

const actionsVerificationSQL = `
 (SELECT v.verdict FROM sage.verification v
   WHERE v.action_log_id = action_log.id
   ORDER BY v.id DESC LIMIT 1) AS verification_verdict,
 (SELECT v.completed_at FROM sage.verification v
   WHERE v.action_log_id = action_log.id
   ORDER BY v.id DESC LIMIT 1) AS verification_completed_at
 FROM sage.action_log`

const queuedActionLedgerSQL = `/* pg_sage */SELECT q.id, q.finding_id,
 COALESCE(q.action_type, ''), q.proposed_sql, q.rollback_sql,
 q.action_risk, q.status, q.proposed_at, q.expires_at,
 COALESCE(q.reason, ''), COALESCE(q.policy_decision, ''),
 COALESCE(q.guardrails, '[]'::jsonb), COALESCE(q.attempt_count, 0),
 q.cooldown_until, COALESCE(q.verification_status, ''),
 COALESCE(q.shadow_toil_minutes, 0)
 FROM sage.action_queue q`

func scanActionRows(rows pgx.Rows) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		var (
			id             int64
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
			attempts       int
			verdict        *string
			verifiedAt     *time.Time
		)
		err := rows.Scan(
			&id, &executedAt, &actionType, &findingID,
			&sqlExecuted, &rollbackSQL, &beforeState,
			&afterState, &outcome, &rollbackReason,
			&measuredAt, &attempts, &verdict, &verifiedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		a := buildActionMap(
			id, executedAt, actionType, findingID,
			sqlExecuted, rollbackSQL, beforeState,
			afterState, outcome, rollbackReason, measuredAt,
		)
		a["attempts"] = attempts
		a["action_risk"] = deriveDisplayActionRisk(sqlExecuted)
		annotateVerification(a, verdict, verifiedAt)
		results = append(results, a)
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}

// deriveDisplayActionRisk classifies an executed action's SQL into a risk
// tier for the UI. Executed actions are always safe/moderate — advisory
// (high_risk) findings never auto-run, so they don't appear here.
func deriveDisplayActionRisk(sql string) string {
	u := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(u, "ALTER SYSTEM"),
		strings.HasPrefix(u, "DROP INDEX"):
		return "moderate"
	default:
		return "safe"
	}
}

func scanQueuedActionLedgerRows(rows pgx.Rows) ([]map[string]any, error) {
	var results []map[string]any
	for rows.Next() {
		action, err := scanQueuedActionLedgerRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, action)
	}
	if results == nil {
		results = []map[string]any{}
	}
	return results, nil
}

func scanQueuedActionLedgerRow(rows pgx.Rows) (map[string]any, error) {
	var id, findingID int
	var actionType, sql, risk, status, reason, policy, verification string
	var rollback *string
	var proposedAt, expiresAt time.Time
	var guardrails []byte
	var attemptCount, shadowToil int
	var cooldownUntil *time.Time
	err := rows.Scan(&id, &findingID, &actionType, &sql, &rollback,
		&risk, &status, &proposedAt, &expiresAt, &reason, &policy,
		&guardrails, &attemptCount, &cooldownUntil, &verification,
		&shadowToil)
	if err != nil {
		return nil, fmt.Errorf("scan queued action ledger: %w", err)
	}
	if actionType == "" {
		actionType = "queued_action"
	}
	return buildQueuedActionLedgerMap(id, findingID, actionType, sql,
		rollback, risk, status, proposedAt, expiresAt, reason, policy,
		decodeStringSlice(guardrails), attemptCount, cooldownUntil,
		verification, shadowToil), nil
}

func buildQueuedActionLedgerMap(
	id, findingID int, actionType, sql string, rollback *string,
	risk, status string, proposedAt, expiresAt time.Time,
	reason, policy string, guardrails []string, attemptCount int,
	cooldownUntil *time.Time, verification string, shadowToil int,
) map[string]any {
	return map[string]any{
		"id":                  strconv.Itoa(id),
		"record_kind":         recordKindQueued,
		"ledger_key":          "queue:" + strconv.Itoa(id),
		"finding_id":          strconv.Itoa(findingID),
		"action_type":         actionType,
		"sql_executed":        sql,
		"rollback_sql":        derefStr(rollback),
		"outcome":             status,
		"status":              status,
		"action_risk":         risk,
		"executed_at":         proposedAt,
		"event_at":            proposedAt,
		"expires_at":          expiresAt,
		"rollback_reason":     reason,
		"policy_decision":     policy,
		"guardrails":          guardrails,
		"attempt_count":       attemptCount,
		"cooldown_until":      cooldownUntil,
		"verification_status": verification,
		"shadow_toil_minutes": shadowToil,
	}
}

func decodeStringSlice(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil
	}
	return values
}

func buildActionMap(
	id int64, executedAt time.Time, actionType string,
	findingID *int64, sqlExecuted string,
	rollbackSQL *string, beforeState, afterState []byte,
	outcome string, rollbackReason *string,
	measuredAt *time.Time,
) map[string]any {
	var before, after any
	if len(beforeState) > 0 {
		_ = json.Unmarshal(beforeState, &before)
	}
	if len(afterState) > 0 {
		_ = json.Unmarshal(afterState, &after)
	}
	var fID *string
	if findingID != nil {
		s := strconv.FormatInt(*findingID, 10)
		fID = &s
	}
	return map[string]any{
		"id":              strconv.FormatInt(id, 10),
		"record_kind":     recordKindExecuted,
		"ledger_key":      "log:" + strconv.FormatInt(id, 10),
		"executed_at":     executedAt,
		"event_at":        executedAt,
		"action_type":     actionType,
		"finding_id":      fID,
		"sql_executed":    sqlExecuted,
		"rollback_sql":    derefStr(rollbackSQL),
		"before_state":    before,
		"after_state":     after,
		"outcome":         outcome,
		"rollback_reason": derefStr(rollbackReason),
		"measured_at":     measuredAt,
	}
}

// annotateVerification attaches the latest durable verification record
// and derives verification_status from it (SURF-12 / G6-B10).
func annotateVerification(
	a map[string]any, verdict *string, completedAt *time.Time,
) {
	if verdict != nil {
		a["verification_verdict"] = *verdict
	}
	if completedAt != nil {
		a["verification_completed_at"] = completedAt
	}
	a["verification_status"] = actionLogVerificationStatus(a)
}

type rollbackBody struct {
	Reason     string `json:"reason"`
	RecordKind string `json:"record_kind"`
}

// decodeRollbackBody parses a rollback request. Rollback resolves ids
// against sage.action_log only, so a request the client labels as
// anything other than an executed action is refused before the
// executor runs (G6-B05). An absent record_kind keeps older API
// clients working; the dashboard always sends it.
func decodeRollbackBody(
	w http.ResponseWriter, r *http.Request,
) (rollbackBody, bool) {
	var body rollbackBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return body, false
	}
	kind := strings.TrimSpace(body.RecordKind)
	if kind != "" && kind != recordKindExecuted {
		jsonError(w, "only executed actions can be rolled back; "+
			"queued proposals must be rejected instead",
			http.StatusBadRequest)
		return body, false
	}
	return body, true
}
