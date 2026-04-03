package notify

import "fmt"

// ActionExecutedEvent creates an event for a successfully executed action.
// Call from executor after an action completes successfully.
func ActionExecutedEvent(
	title, sql, database string,
) Event {
	return Event{
		Type:     "action_executed",
		Severity: "info",
		Subject:  fmt.Sprintf("Action executed: %s", title),
		Body: fmt.Sprintf(
			"Database: %s\nSQL: %s", database, sql),
		Data: map[string]any{
			"title":    title,
			"sql":      sql,
			"database": database,
		},
	}
}

// ActionFailedEvent creates an event for a failed action execution.
// Call from executor when an action fails.
func ActionFailedEvent(
	title, sql, database, errMsg string,
) Event {
	return Event{
		Type:     "action_failed",
		Severity: "warning",
		Subject:  fmt.Sprintf("Action failed: %s", title),
		Body: fmt.Sprintf(
			"Database: %s\nSQL: %s\nError: %s",
			database, sql, errMsg),
		Data: map[string]any{
			"title":    title,
			"sql":      sql,
			"database": database,
			"error":    errMsg,
		},
	}
}

// ApprovalNeededEvent creates an event when an action awaits approval.
// Call from executor when an action is queued for approval.
func ApprovalNeededEvent(
	title, sql, database, risk string,
) Event {
	return Event{
		Type:     "approval_needed",
		Severity: "warning",
		Subject:  fmt.Sprintf("Approval needed: %s", title),
		Body: fmt.Sprintf(
			"Database: %s\nRisk: %s\nSQL: %s",
			database, risk, sql),
		Data: map[string]any{
			"title":    title,
			"sql":      sql,
			"database": database,
			"risk":     risk,
		},
	}
}

// FindingCriticalEvent creates an event for a new critical finding.
// Call from analyzer when a critical severity finding is detected.
func FindingCriticalEvent(
	title, detail, database string,
) Event {
	return Event{
		Type:     "finding_critical",
		Severity: "critical",
		Subject:  fmt.Sprintf("Critical finding: %s", title),
		Body: fmt.Sprintf(
			"Database: %s\nDetail: %s", database, detail),
		Data: map[string]any{
			"title":    title,
			"detail":   detail,
			"database": database,
		},
	}
}

// QueryRewriteEvent creates an event when the tuner suggests a
// query rewrite. Call from analyzer when a tuning finding includes
// a suggested_rewrite in its detail.
func QueryRewriteEvent(
	title, query, rewrite, rationale, database string,
) Event {
	return Event{
		Type:     "query_rewrite_suggested",
		Severity: "warning",
		Subject:  fmt.Sprintf("Query rewrite suggested: %s", title),
		Body: fmt.Sprintf(
			"Database: %s\nOriginal: %s\nRewrite: %s\n"+
				"Rationale: %s",
			database, query, rewrite, rationale),
		Data: map[string]any{
			"title":     title,
			"query":     query,
			"rewrite":   rewrite,
			"rationale": rationale,
			"database":  database,
		},
	}
}
