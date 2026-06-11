package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/store"
)

func casesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		if mgr == nil {
			writeCasesResponse(w, database, []cases.Case{})
			return
		}

		projected, err := queryProjectedCases(r.Context(), mgr, database)
		if err != nil {
			slog.Error("query cases failed", "error", err)
			jsonError(w, "failed to query cases", http.StatusInternalServerError)
			return
		}
		writeCasesResponse(w, database, projected)
	}
}

func shadowReportHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		if mgr == nil {
			jsonResponse(w, cases.BuildShadowReport(nil))
			return
		}

		projected, err := queryProjectedCases(r.Context(), mgr, database)
		if err != nil {
			slog.Error("query shadow report failed", "error", err)
			jsonError(w, "failed to query shadow report", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, cases.BuildShadowReport(projected))
	}
}

func writeCasesResponse(w http.ResponseWriter, database string, projected []cases.Case) {
	if projected == nil {
		projected = []cases.Case{}
	}
	jsonResponse(w, map[string]any{
		"database": database,
		"cases":    projected,
		"total":    len(projected),
	})
}

func queryProjectedCases(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	database string,
) ([]cases.Case, error) {
	filters := fleet.FindingFilters{Status: "open", Limit: 500, Sort: "severity", Order: "desc"}
	pools := poolsForDatabaseSelection(mgr, database)
	out := make([]cases.Case, 0)
	for _, selected := range pools {
		rows, _, err := queryFindings(ctx, selected.pool, filters, selected.name)
		if err != nil {
			return nil, err
		}
		selectedCases := make([]cases.Case, 0, len(rows))
		findingIDs := make([]int, 0, len(rows))
		for _, row := range rows {
			// Never surface pg_sage's own monitoring queries as cases —
			// including historical findings captured before queries were
			// tagged (recognized by their catalog-read signature).
			if selfmonitor.IsFinding(selfmonitor.FindingFields{
				ObjectIdentifier: stringValue(row["object_identifier"]),
				Title:            stringValue(row["title"]),
				Detail:           detailMap(row["detail"]),
				RecommendedSQL:   stringValue(row["recommended_sql"]),
				RollbackSQL:      stringValue(row["rollback_sql"]),
			}) {
				continue
			}
			projected := cases.ProjectFinding(sourceFindingFromMap(row))
			enrichCaseActionPolicies(&projected, mgr, selected.name)
			if id, ok := caseFindingID(projected); ok {
				findingIDs = append(findingIDs, id)
			}
			selectedCases = append(selectedCases, projected)
		}
		enrichCaseActionTimelines(
			ctx, selectedCases, selected.pool,
			store.NewActionStore(selected.pool), findingIDs,
		)
		out = append(out, selectedCases...)
		incidentCases, err := queryIncidentCases(ctx, mgr, selected)
		if err != nil {
			return nil, err
		}
		out = append(out, incidentCases...)
		queryHintCases, err := queryHintCases(ctx, selected)
		if err != nil {
			return nil, err
		}
		out = append(out, queryHintCases...)
	}
	return out, nil
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
	mgr *fleet.DatabaseManager,
	selected namedPool,
) ([]cases.Case, error) {
	rows, err := queryActiveIncidents(ctx, selected.pool, "")
	if err != nil {
		return nil, err
	}
	out := make([]cases.Case, 0, len(rows))
	for _, row := range rows {
		annotateIncidentFleetDatabase(row, selected.name)
		projected := cases.ProjectIncident(sourceIncidentFromMap(row))
		enrichCaseActionPolicies(&projected, mgr, selected.name)
		out = append(out, projected)
	}
	return out, nil
}

func queryHintCases(
	ctx context.Context,
	selected namedPool,
) ([]cases.Case, error) {
	rows, err := queryQueryHints(ctx, selected.pool)
	if err != nil {
		return nil, err
	}
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
	return out, nil
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
	psRows, err := pool.Query(ctx,
		`/* pg_sage */ SELECT queryid, min(query)
		 FROM pg_stat_statements
		 WHERE dbid = (
		   SELECT oid FROM pg_database WHERE datname = current_database()
		 )
		   AND queryid = ANY($1::bigint[])
		 GROUP BY queryid`,
		queryIDs,
	)
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
	default:
		return 0
	}
}

func filterSelfMonitoringHintRows(
	rows []map[string]any,
) []map[string]any {
	out := rows[:0]
	for _, row := range rows {
		if selfmonitor.IsQueryText(stringValue(row["query_text"])) {
			continue
		}
		out = append(out, row)
	}
	return out
}

func enrichCaseActionTimeline(
	ctx context.Context,
	c *cases.Case,
	pool *pgxpool.Pool,
	actionStore *store.ActionStore,
) {
	if len(c.SourceIDs) == 0 {
		return
	}
	findingID, err := strconv.Atoi(c.SourceIDs[0])
	if err != nil || findingID <= 0 {
		return
	}
	if actionStore != nil {
		queued, err := actionStore.ListLedgerByFinding(ctx, findingID)
		if err == nil {
			now := time.Now().UTC()
			for _, action := range queued {
				c.Actions = append(c.Actions,
					caseActionFromQueuedAction(action, now))
			}
		}
	}
	logged, err := queryActionLogsByFinding(ctx, pool, findingID)
	if err != nil {
		return
	}
	for _, action := range logged {
		c.Actions = append(c.Actions, caseActionFromActionLog(action))
	}
}

func enrichCaseActionTimelines(
	ctx context.Context,
	projected []cases.Case,
	pool *pgxpool.Pool,
	actionStore *store.ActionStore,
	findingIDs []int,
) {
	if len(findingIDs) == 0 {
		return
	}
	now := time.Now().UTC()
	queuedByID := map[int][]store.QueuedAction{}
	loggedByID := map[int][]map[string]any{}
	if actionStore != nil {
		if queued, err := actionStore.ListLedgerByFindingIDs(
			ctx, findingIDs,
		); err == nil {
			queuedByID = queued
		}
	}
	if logged, err := queryActionLogsByFindingIDs(
		ctx, pool, findingIDs,
	); err == nil {
		loggedByID = logged
	}
	for i := range projected {
		findingID, ok := caseFindingID(projected[i])
		if !ok {
			continue
		}
		for _, action := range queuedByID[findingID] {
			projected[i].Actions = append(
				projected[i].Actions,
				caseActionFromQueuedAction(action, now),
			)
		}
		for _, action := range loggedByID[findingID] {
			projected[i].Actions = append(
				projected[i].Actions,
				caseActionFromActionLog(action),
			)
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

func queryActionLogsByFinding(
	ctx context.Context,
	pool *pgxpool.Pool,
	findingID int,
) ([]map[string]any, error) {
	if pool == nil {
		return []map[string]any{}, nil
	}
	rows, err := pool.Query(ctx, actionsSelectSQLPrefix+
		` WHERE finding_id = $1 ORDER BY executed_at DESC LIMIT 20`,
		findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanActionRows(rows)
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
	rows, err := pool.Query(ctx, actionsSelectSQLPrefix+`
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
 )
 ORDER BY finding_id, executed_at DESC, id DESC`, findingIDs)
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

func actionLogVerificationStatus(row map[string]any) string {
	outcome := stringValue(row["outcome"])
	if outcome == "success" && row["measured_at"] != nil {
		return "verified"
	}
	if outcome == "pending" || outcome == "monitoring" {
		return "monitoring"
	}
	if outcome == "failed" || outcome == "rollback_failed" {
		return "failed"
	}
	if outcome == "rolled_back" {
		return "rolled_back"
	}
	if outcome == "success" {
		return "verified"
	}
	return "not_started"
}

func enrichCaseActionPolicies(
	c *cases.Case,
	mgr *fleet.DatabaseManager,
	databaseName string,
) {
	policyContext := casePolicyContext(mgr, databaseName)
	for i := range c.ActionCandidates {
		candidate := &c.ActionCandidates[i]
		contract, ok := executor.ContractForActionType(candidate.ActionType)
		if !ok {
			candidate.BlockedReason = "unknown action type"
			continue
		}
		decision := executor.EvaluateActionPolicy(contract, policyContext)
		candidate.PolicyDecision = &cases.ActionPolicyDecision{
			Decision:                  decision.Decision,
			RiskTier:                  decision.RiskTier,
			RequiresApproval:          decision.RequiresApproval,
			RequiresMaintenanceWindow: decision.RequiresMaintenanceWindow,
			BlockedReason:             decision.BlockedReason,
			Guardrails:                decision.Guardrails,
			Provider:                  decision.Provider,
		}
		candidate.Guardrails = decision.Guardrails
		candidate.RequiresApproval = decision.RequiresApproval
		candidate.RequiresMaintenanceWindow = decision.RequiresMaintenanceWindow
		if decision.BlockedReason != "" {
			candidate.BlockedReason = decision.BlockedReason
		}
	}
}

func casePolicyContext(
	mgr *fleet.DatabaseManager,
	databaseName string,
) executor.ActionPolicyContext {
	cfg := &config.Config{}
	if mgr != nil && mgr.Config() != nil {
		copied := *mgr.Config()
		cfg = &copied
	}
	mode := "auto"
	stopped := false
	isReplica := false
	if mgr != nil {
		if inst := mgr.GetInstance(databaseName); inst != nil {
			mode = executionModeForInstance(cfg, inst)
			stopped = inst.Stopped
			snap := inst.SnapshotStatus()
			if snap.Platform != "" {
				cfg.CloudEnvironment = snap.Platform
			}
			if snap.Capabilities.Provider != "" {
				cfg.CloudEnvironment = snap.Capabilities.Provider
			}
			isReplica = snap.Capabilities.IsReplica
			if inst.Config.TrustLevel != "" {
				cfg.Trust.Level = inst.Config.TrustLevel
			}
		}
	}
	return executor.ActionPolicyContext{
		Config:          cfg,
		ExecutionMode:   mode,
		RampStart:       rampStartForPolicy(cfg),
		IsReplica:       isReplica,
		EmergencyStop:   stopped,
		SafeActionLimit: 3,
	}
}

func executionModeForInstance(
	cfg *config.Config,
	inst *fleet.DatabaseInstance,
) string {
	if inst.Config.ExecutionMode != "" {
		return inst.Config.ExecutionMode
	}
	if cfg != nil && cfg.Defaults.ExecutionMode != "" {
		return cfg.Defaults.ExecutionMode
	}
	return "auto"
}

func rampStartForPolicy(cfg *config.Config) time.Time {
	if cfg == nil || cfg.Trust.RampStart == "" {
		return time.Now().Add(-365 * 24 * time.Hour)
	}
	parsed, err := time.Parse(time.RFC3339, cfg.Trust.RampStart)
	if err != nil {
		return time.Time{}
	}
	return parsed
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
	}
}

func sourceQueryHintFromMap(row map[string]any) cases.SourceQueryHint {
	return cases.SourceQueryHint{
		QueryID:          int64(floatValue(row["queryid"])),
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

func incidentChainFromAny(v any) []cases.IncidentChainLink {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]cases.IncidentChainLink, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, cases.IncidentChainLink{
			Order:       int(floatValue(m["order"])),
			Signal:      stringValue(m["signal"]),
			Description: stringValue(m["description"]),
			Evidence:    stringValue(m["evidence"]),
		})
	}
	return out
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
