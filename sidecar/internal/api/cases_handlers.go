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
		for _, row := range rows {
			projected := cases.ProjectFinding(sourceFindingFromMap(row))
			enrichCaseActionPolicies(&projected, mgr, selected.name)
			enrichCaseActionTimeline(
				ctx, &projected, selected.pool,
				store.NewActionStore(selected.pool),
			)
			out = append(out, projected)
		}
	}
	return out, nil
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
		Detail:           detailMap(row["detail"]),
	}
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
