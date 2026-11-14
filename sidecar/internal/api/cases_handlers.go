package api

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/store"
)

// queryDatabaseCases projects one database's open findings, active
// incidents and query hints, each read one row past its cap so the cut is
// reported. The finding, incident and query-hint candidates share one
// standing-policy explanation for the database; a candidate past its
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
	incidentCases, cut, err := queryIncidentCases(ctx, selected)
	if err != nil {
		return res, err
	}
	res.truncated.Incidents = cut
	hintCases, cut, err := queryHintCases(ctx, selected)
	if err != nil {
		return res, err
	}
	res.truncated.QueryHints = cut
	out := append(append(findingCases, incidentCases...), hintCases...)
	enrichCasesActionPolicies(ctx, out, mgr.GetInstance(selected.name))
	enrichFindingCases(ctx, out[:len(findingCases)], selected, findingIDs)
	cases.MarkExpiredCandidates(out, time.Now().UTC())
	res.cases = out
	return res, nil
}

// queryFindingCases projects one database's open findings (one bounded
// index-ordered read; the cases view needs no total), skipping pg_sage's
// own monitoring queries. cut reports open findings past the cap.
func queryFindingCases(
	ctx context.Context, selected namedPool,
) (out []cases.Case, findingIDs []int, cut bool, err error) {
	filters := fleet.FindingFilters{
		Status: "open", Limit: caseFindingsCap, Sort: "severity", Order: "desc",
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
