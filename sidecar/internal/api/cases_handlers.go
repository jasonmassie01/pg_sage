package api

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/workload"
)

// queryDatabaseCases projects one database's open findings, active
// incidents and query hints (only the findings under a finding source
// filter), each read one row past its cap so the cut is reported. The
// finding, incident and query-hint candidates share one standing-policy
// explanation for the database; a candidate past its
// expires_at is blocked as evidence_expired.
func queryDatabaseCases(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	selected namedPool,
) (databaseCases, error) {
	var res databaseCases
	findingCases, findingIDs, cut, err := queryFindingCases(ctx, selected)
	if err != nil {
		return res, err
	}
	res.truncated.Findings = cut
	otherCases, err := queryOtherCases(ctx, selected, &res.truncated)
	if err != nil {
		return res, err
	}
	out := append(findingCases, otherCases...)
	enrichCasesActionPolicies(ctx, out, mgr.GetInstance(selected.name))
	enrichFindingCases(ctx, out[:len(findingCases)], selected, findingIDs)
	cases.MarkExpiredCandidates(out, time.Now().UTC())
	res.cases = out
	return res, nil
}

// queryFindingCases projects one database's open findings (one bounded
// index-ordered read; the cases view needs no total), skipping pg_sage's
// own monitoring queries. The request's finding source filters the read
// before the cap. cut reports open findings past the cap.
func queryFindingCases(
	ctx context.Context, selected namedPool,
) (out []cases.Case, findingIDs []int, cut bool, err error) {
	filters := fleet.FindingFilters{
		Status: "open", Limit: caseFindingsCap, Sort: "severity", Order: "desc",
		Source: findingSourceFrom(ctx),
	}
	page, err := queryFindingsPage(ctx, selected.pool, filters,
		listPage{Limit: filters.Limit}, selected.name, false)
	if err != nil {
		return nil, nil, false, err
	}
	rows, next := mergePage(page.rows, listPage{Limit: filters.Limit}, true,
		filters.Sort, filters.Order)
	out, findingIDs = projectFindingRows(rows)
	return out, findingIDs, next != "", nil
}

// projectFindingRows projects finding rows into cases and their finding
// ids, skipping pg_sage's own monitoring queries.
func projectFindingRows(rows []map[string]any) ([]cases.Case, []int) {
	out := make([]cases.Case, 0, len(rows))
	findingIDs := make([]int, 0, len(rows))
	for _, row := range rows {
		// Never surface pg_sage's own monitoring queries as cases —
		// including historical findings captured before queries were
		// tagged (recognized by their catalog-read signature).
		// Never surface findings about pg_sage's own objects or queries.
		if selfmonitor.IsFinding(selfmonitor.FindingFields{
			Category:         stringValue(row["category"]),
			ObjectType:       stringValue(row["object_type"]),
			ObjectIdentifier: stringValue(row["object_identifier"]),
			Title:            stringValue(row["title"]),
			Detail:           detailMap(row["detail"]),
			RecommendedSQL:   stringValue(row["recommended_sql"]),
			RollbackSQL:      stringValue(row["rollback_sql"]),
		}) {
			continue
		}
		projected := cases.ProjectFinding(sourceFindingFromMap(row), executor.ClassifySQL)
		if id, ok := caseFindingID(projected); ok {
			findingIDs = append(findingIDs, id)
		}
		out = append(out, projected)
	}
	return out, findingIDs
}

func caseFindingID(c cases.Case) (int, bool) {
	if len(c.SourceIDs) == 0 {
		return 0, false
	}
	findingID, err := strconv.Atoi(c.SourceIDs[0])
	return findingID, err == nil && findingID > 0
}

func queryIncidentCases(
	ctx context.Context,
	selected namedPool,
) ([]cases.Case, bool, error) {
	rows, err := queryActiveIncidentsLimit(ctx, selected.pool, "", caseIncidentsCap+1)
	if err != nil {
		return nil, false, err
	}
	cut := len(rows) > caseIncidentsCap
	if cut {
		rows = rows[:caseIncidentsCap]
	}
	out := projectIncidentRows(rows, selected.name)
	enrichIncidentRecurrence(ctx, selected.pool, out)
	return out, cut, nil
}

func projectIncidentRows(rows []map[string]any, database string) []cases.Case {
	out := make([]cases.Case, 0, len(rows))
	for _, row := range rows {
		annotateIncidentFleetDatabase(row, database)
		out = append(out, cases.ProjectIncident(sourceIncidentFromMap(row)))
	}
	return out
}

func queryHintCases(
	ctx context.Context,
	selected namedPool,
) ([]cases.Case, bool, error) {
	rows, err := queryQueryHintsLimit(ctx, selected.pool, caseQueryHintsCap+1, "")
	if err != nil {
		return nil, false, err
	}
	cut := len(rows) > caseQueryHintsCap
	if cut {
		rows = rows[:caseQueryHintsCap]
	}
	return projectHintRows(ctx, selected, rows), cut, nil
}

func projectHintRows(
	ctx context.Context, selected namedPool, rows []map[string]any,
) []cases.Case {
	annotateQueryHintTexts(ctx, selected.pool, rows)
	rows = filterSelfMonitoringHintRows(rows)
	out := make([]cases.Case, 0, len(rows))
	for _, row := range rows {
		if stringValue(row["status"]) == "retired" {
			continue
		}
		row["database_name"] = selected.name
		out = append(out, cases.ProjectQueryHint(sourceQueryHintFromMap(row)))
	}
	return out
}

func annotateQueryHintTexts(
	ctx context.Context,
	pool *pgxpool.Pool,
	rows []map[string]any,
) {
	queryIDs := uniqueQueryHintIDs(rows)
	if len(queryIDs) == 0 {
		return
	}
	rel, err := extschema.StatementsRel(ctx, pool, "pg_stat_statements")
	var psRows pgx.Rows
	if err == nil {
		psRows, err = pool.Query(ctx,
			`/* pg_sage */ SELECT queryid, min(query)
		 FROM `+rel+`
		 WHERE dbid = (
		   SELECT oid FROM pg_database WHERE datname = current_database()
		 )
		   AND queryid = ANY($1::bigint[])
		 GROUP BY queryid`,
			queryIDs,
		)
	}
	if err != nil {
		// Don't fail silently: without query_text the self-monitoring
		// filter downstream can't recognize pg_sage's own hints, so they
		// would leak into the UI. Surface the cause (D2).
		slog.Warn("query-hint text annotation failed",
			"error", err)
		return
	}
	defer psRows.Close()

	textByID := make(map[int64]string, len(queryIDs))
	for psRows.Next() {
		var queryID int64
		var queryText string
		if err := psRows.Scan(&queryID, &queryText); err == nil {
			textByID[queryID] = queryText
		}
	}
	for _, row := range rows {
		if queryText := textByID[queryHintID(row)]; queryText != "" {
			row["query_text"] = queryText
		}
	}
}

func uniqueQueryHintIDs(rows []map[string]any) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, row := range rows {
		queryID := queryHintID(row)
		if queryID == 0 || seen[queryID] {
			continue
		}
		seen[queryID] = true
		out = append(out, queryID)
	}
	return out
}

func queryHintID(row map[string]any) int64 {
	// queryid is a 64-bit pg_stat_statements hash and is stored as an
	// int64 (see scanQueryHintRows). It must NOT go through float64:
	// values beyond 2^53 lose precision, so the ANY($1::bigint[]) lookup
	// in annotateQueryHintTexts would miss and the self-monitoring
	// filter would silently fail (D1).
	return int64Value(row["queryid"])
}

// int64Value extracts an int64 from a row value without a lossy float
// round-trip. Integer types are returned exactly; float types are
// truncated only as a last resort for values that arrived as floats.
func int64Value(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

func filterSelfMonitoringHintRows(
	rows []map[string]any,
) []map[string]any {
	out := rows[:0]
	for _, row := range rows {
		if workload.Excluded(stringValue(row["query_text"])) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// enrichCaseActionTimelines attaches each finding case's queued and
// executed actions. A history that cannot be read is logged and marks the
// cases actions_unavailable rather than reading as "no actions".
func enrichCaseActionTimelines(
	ctx context.Context,
	projected []cases.Case,
	selected namedPool,
	actionStore *store.ActionStore,
	findingIDs []int,
) {
	if len(findingIDs) == 0 {
		return
	}
	now := time.Now().UTC()
	queuedByID := map[int][]store.QueuedAction{}
	unavailable := false
	if actionStore != nil {
		queued, err := actionStore.ListLedgerByFindingIDs(ctx, findingIDs)
		if err != nil {
			slog.Warn("case action queue read failed",
				"database", selected.name, "error", err)
			unavailable = true
		} else {
			queuedByID = queued
		}
	}
	loggedByID, err := queryActionLogsByFindingIDs(ctx, selected.pool, findingIDs)
	if err != nil {
		slog.Warn("case action log read failed",
			"database", selected.name, "error", err)
		unavailable = true
	}
	for i := range projected {
		findingID, ok := caseFindingID(projected[i])
		if !ok {
			continue
		}
		projected[i].ActionsUnavailable = unavailable
		for _, action := range queuedByID[findingID] {
			projected[i].Actions = append(projected[i].Actions,
				caseActionFromQueuedAction(action, now))
		}
		for _, action := range loggedByID[findingID] {
			projected[i].Actions = append(projected[i].Actions,
				caseActionFromActionLog(action))
		}
	}
}

func caseActionFromQueuedAction(
	action store.QueuedAction,
	now time.Time,
) cases.CaseAction {
	decision := store.EvaluateActionLifecycle(store.ActionLifecycleInput{
		Status:                 action.Status,
		ExpiresAt:              action.ExpiresAt,
		CooldownUntil:          action.CooldownUntil,
		AttemptCount:           action.AttemptCount,
		MaxAttempts:            3,
		FailureFingerprint:     action.FailureFingerprint,
		LastFailureFingerprint: action.LastFailureFingerprint,
		EvidencePresent:        true,
		Now:                    now,
	})
	proposedAt := action.ProposedAt
	blockedReason := decision.BlockedReason
	if blockedReason == "" {
		blockedReason = action.Reason
	}
	return cases.CaseAction{
		ID:                 fmt.Sprintf("queue:%d", action.ID),
		Type:               queuedActionType(action),
		RiskTier:           action.ActionRisk,
		Status:             action.Status,
		PolicyDecision:     action.PolicyDecision,
		LifecycleState:     decision.State,
		BlockedReason:      blockedReason,
		VerificationStatus: action.VerificationStatus,
		AttemptCount:       action.AttemptCount,
		ShadowToilMinutes:  action.ShadowToilMinutes,
		Guardrails:         action.Guardrails,
		ProposedAt:         &proposedAt,
		ExpiresAt:          action.ExpiresAt,
		CooldownUntil:      action.CooldownUntil,
	}
}

func queuedActionType(action store.QueuedAction) string {
	if action.ActionType != "" {
		return action.ActionType
	}
	return action.ActionRisk
}

func queryActionLogsByFindingIDs(
	ctx context.Context,
	pool *pgxpool.Pool,
	findingIDs []int,
) (map[int][]map[string]any, error) {
	out := make(map[int][]map[string]any, len(findingIDs))
	if pool == nil || len(findingIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, actionsWithAttemptsSQL(`SELECT * FROM sage.action_log
 WHERE id IN (
     SELECT id
     FROM (
         SELECT id, finding_id,
                row_number() OVER (
                    PARTITION BY finding_id
                    ORDER BY executed_at DESC, id DESC
                ) AS rn
         FROM sage.action_log
         WHERE finding_id = ANY($1)
     ) ranked
     WHERE rn <= 20
 )`, "", "\n ORDER BY finding_id, executed_at DESC, action_log.id DESC"), findingIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	actions, err := scanActionRows(rows)
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		rawID, ok := action["finding_id"].(*string)
		if !ok || rawID == nil {
			continue
		}
		findingID, err := strconv.Atoi(*rawID)
		if err != nil {
			continue
		}
		out[findingID] = append(out[findingID], action)
	}
	return out, nil
}

func caseActionFromActionLog(row map[string]any) cases.CaseAction {
	executedAt := timeFromMap(row, "executed_at")
	action := cases.CaseAction{
		ID:                 "log:" + stringValue(row["id"]),
		Type:               stringValue(row["action_type"]),
		Status:             stringValue(row["outcome"]),
		LifecycleState:     "executed",
		VerificationStatus: actionLogVerificationStatus(row),
		ProposedAt:         &executedAt,
	}
	return action
}

// actionLogVerificationStatus reports what is actually known about an
// executed action. A successful SQL return is only "applied"; the
// action is "verified" only when the durable sage.verification record
// completed with verdict=success (SURF-12 / G6-B10, matching the value
// service's credit rule).
func actionLogVerificationStatus(row map[string]any) string {
	switch stringValue(row["outcome"]) {
	case "rolled_back", "reverted":
		return "reverted"
	case "failed", "rollback_failed":
		return "failed"
	case "pending", "monitoring":
		return "pending"
	case "success":
		return durableVerificationStatus(row)
	default:
		return "not_started"
	}
}

func durableVerificationStatus(row map[string]any) string {
	verdict := stringValue(row["verification_verdict"])
	completed := row["verification_completed_at"] != nil
	switch {
	case verdict == "":
		return "applied"
	case verdict == "success" && completed:
		return "verified"
	case verdict == "revert":
		return "reverted"
	case verdict == "failed":
		return "failed"
	case verdict == "unverifiable":
		return "inconclusive"
	default:
		return "pending"
	}
}

func sourceFindingFromMap(row map[string]any) cases.SourceFinding {
	return cases.SourceFinding{
		ID:               stringValue(row["id"]),
		DatabaseName:     stringValue(row["database_name"]),
		Category:         stringValue(row["category"]),
		Severity:         cases.Severity(stringValue(row["severity"])),
		ObjectType:       stringValue(row["object_type"]),
		ObjectIdentifier: stringValue(row["object_identifier"]),
		RuleID:           stringValue(row["rule_id"]),
		Title:            stringValue(row["title"]),
		Recommendation:   stringValue(row["recommendation"]),
		RecommendedSQL:   stringValue(row["recommended_sql"]),
		RollbackSQL:      stringValue(row["rollback_sql"]),
		Detail:           detailMap(row["detail"]),
		ObservedAt:       timeFromMap(row, "last_seen"),
	}
}

func sourceIncidentFromMap(row map[string]any) cases.SourceIncident {
	return cases.SourceIncident{
		ID:              stringValue(row["id"]),
		DatabaseName:    stringValue(row["database_name"]),
		Severity:        cases.Severity(stringValue(row["severity"])),
		RootCause:       stringValue(row["root_cause"]),
		CausalChain:     incidentChainFromAny(row["causal_chain"]),
		AffectedObjects: stringSliceFromAny(row["affected_objects"]),
		SignalIDs:       stringSliceFromAny(row["signal_ids"]),
		RecommendedSQL:  stringValue(row["recommended_sql"]),
		ActionRisk:      stringValue(row["action_risk"]),
		Source:          stringValue(row["source"]),
		Confidence:      floatValue(row["confidence"]),
		DetectedAt:      timeFromMap(row, "detected_at"),
		LastDetectedAt:  timeFromMap(row, "last_detected_at"),
		ResolvedAt:      timePtrFromMap(row, "resolved_at"),
		OccurrenceCount: int(floatValue(row["occurrence_count"])),

		PreviousIncidentID: stringValue(row["previous_incident_id"]),
	}
}

func sourceQueryHintFromMap(row map[string]any) cases.SourceQueryHint {
	return cases.SourceQueryHint{
		QueryID:          int64Value(row["queryid"]),
		DatabaseName:     stringValue(row["database_name"]),
		HintText:         stringValue(row["hint_text"]),
		Symptom:          stringValue(row["symptom"]),
		Status:           stringValue(row["status"]),
		CreatedAt:        timeFromMap(row, "created_at"),
		BeforeCost:       floatPtrFromAny(row["before_cost"]),
		AfterCost:        floatPtrFromAny(row["after_cost"]),
		SuggestedRewrite: stringValue(row["suggested_rewrite"]),
		RewriteRationale: stringValue(row["rewrite_rationale"]),
		VerifiedAt:       timePtrFromMap(row, "verified_at"),
		RolledBackAt:     timePtrFromMap(row, "rolled_back_at"),
	}
}

func stringSliceFromAny(v any) []string {
	switch items := v.(type) {
	case []string:
		return append([]string(nil), items...)
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if s := stringValue(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func timePtrFromMap(row map[string]any, key string) *time.Time {
	if row[key] == nil {
		return nil
	}
	t := timeFromMap(row, key)
	if t.IsZero() {
		return nil
	}
	return &t
}

func detailMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{"raw": value}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func floatValue(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case int32:
		return float64(v)
	default:
		return 0
	}
}

func floatPtrFromAny(value any) *float64 {
	if value == nil {
		return nil
	}
	f := floatValue(value)
	return &f
}
