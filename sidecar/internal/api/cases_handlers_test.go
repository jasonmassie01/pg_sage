package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func TestCasesHandlerRejectsBadDatabaseParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/cases?database=bad'db", nil)
	rr := httptest.NewRecorder()

	casesHandler(nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestCasesHandlerEmptyWhenNoFleet(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/cases", nil)
	rr := httptest.NewRecorder()

	casesHandler(nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["total"].(float64) != 0 {
		t.Fatalf("total = %v, want 0", body["total"])
	}
}

func TestCasesRouteRegistered(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/cases")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := decodeJSON(t, w)
	if body["total"].(float64) != 0 {
		t.Fatalf("total = %v, want 0", body["total"])
	}
}

func TestShadowReportHandlerEmptyWhenNoFleet(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/shadow-report", nil)
	rr := httptest.NewRecorder()

	shadowReportHandler(nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["total_cases"].(float64) != 0 {
		t.Fatalf("total_cases = %v, want 0", body["total_cases"])
	}
}

func TestEnrichCaseActionPoliciesAddsDeterministicDecision(t *testing.T) {
	cfg := &config.Config{
		Mode:             "fleet",
		CloudEnvironment: "cloud-sql",
		Trust: config.TrustConfig{
			Level:     "autonomous",
			Tier3Safe: true,
			RampStart: time.Now().
				Add(-10 * 24 * time.Hour).
				Format(time.RFC3339),
		},
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "prod",
		Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
	})
	c := cases.Case{
		DatabaseName: "prod",
		ActionCandidates: []cases.ActionCandidate{{
			ActionType: "analyze_table",
			RiskTier:   "safe",
		}},
	}

	enrichCaseActionPolicies(&c, mgr, "prod")

	decision := c.ActionCandidates[0].PolicyDecision
	if decision == nil {
		t.Fatal("missing policy decision")
	}
	if decision.Decision != executor.PolicyDecisionExecute {
		t.Fatalf("Decision = %q, want execute", decision.Decision)
	}
	if len(c.ActionCandidates[0].Guardrails) == 0 {
		t.Fatalf("expected candidate guardrails")
	}
}

func TestEnrichCaseActionPoliciesShowsApprovalAndWindowBlock(t *testing.T) {
	cfg := &config.Config{
		Mode:             "fleet",
		CloudEnvironment: "postgres",
		Trust: config.TrustConfig{
			Level:             "autonomous",
			Tier3Moderate:     true,
			MaintenanceWindow: "0 2 * * *",
			RampStart: time.Now().
				Add(-40 * 24 * time.Hour).
				Format(time.RFC3339),
		},
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "prod",
		Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
	})
	c := cases.Case{
		DatabaseName: "prod",
		ActionCandidates: []cases.ActionCandidate{{
			ActionType: "create_index_concurrently",
			RiskTier:   "moderate",
		}},
	}

	enrichCaseActionPolicies(&c, mgr, "prod")

	candidate := c.ActionCandidates[0]
	if candidate.PolicyDecision == nil {
		t.Fatal("missing policy decision")
	}
	if !candidate.RequiresApproval || !candidate.RequiresMaintenanceWindow {
		t.Fatalf("expected approval and window flags: %#v", candidate)
	}
	if candidate.BlockedReason == "" {
		t.Fatalf("expected blocked reason outside maintenance window")
	}
}

func TestCasePolicyContextUsesInstancePlatform(t *testing.T) {
	cfg := &config.Config{
		Mode:             "fleet",
		CloudEnvironment: "postgres",
		Trust: config.TrustConfig{
			Level:     "autonomous",
			Tier3Safe: true,
			RampStart: time.Now().
				Add(-10 * 24 * time.Hour).
				Format(time.RFC3339),
		},
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "prod",
		Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Status: &fleet.InstanceStatus{
			Platform: "cloud-sql",
		},
	})

	got := casePolicyContext(mgr, "prod")

	if got.Config.CloudEnvironment != "cloud-sql" {
		t.Fatalf("CloudEnvironment = %q, want cloud-sql",
			got.Config.CloudEnvironment)
	}
}

func TestCasePolicyContextBlocksReplicaFromCapabilities(t *testing.T) {
	cfg := &config.Config{
		Mode:             "fleet",
		CloudEnvironment: "postgres",
		Trust: config.TrustConfig{
			Level:     "autonomous",
			Tier3Safe: true,
			RampStart: time.Now().
				Add(-10 * 24 * time.Hour).
				Format(time.RFC3339),
		},
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "prod",
		Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Status: &fleet.InstanceStatus{
			Platform: "postgres",
			Capabilities: fleet.ProviderCapabilities{
				Provider:  "postgres",
				IsReplica: true,
			},
		},
	})
	c := cases.Case{
		DatabaseName: "prod",
		ActionCandidates: []cases.ActionCandidate{{
			ActionType: "analyze_table",
			RiskTier:   "safe",
		}},
	}

	enrichCaseActionPolicies(&c, mgr, "prod")

	got := c.ActionCandidates[0].BlockedReason
	if got != "target database is a replica" {
		t.Fatalf("BlockedReason = %q", got)
	}
}

func TestSourceIncidentFromMapProjectsPlaybookCandidate(t *testing.T) {
	row := map[string]any{
		"id":               "inc-1",
		"database_name":    "prod",
		"severity":         "warning",
		"root_cause":       "Idle-in-transaction PID 12345 blocks vacuum",
		"signal_ids":       []any{"vacuum_blocked"},
		"affected_objects": []any{"public.orders"},
		"recommended_sql":  "SELECT pg_terminate_backend(12345)",
		"action_risk":      "high_risk",
		"source":           "rca",
		"confidence":       0.92,
		"occurrence_count": float64(2),
		"detected_at":      time.Now().UTC().Add(-time.Hour),
		"last_detected_at": time.Now().UTC(),
		"causal_chain": []any{map[string]any{
			"order":       float64(1),
			"signal":      "vacuum_blocked",
			"description": "blocked by idle transaction",
			"evidence":    "blocker pid 12345",
		}},
	}

	incident := sourceIncidentFromMap(row)
	projected := cases.ProjectIncident(incident)

	if projected.SourceType != cases.SourceIncidentType {
		t.Fatalf("SourceType = %q", projected.SourceType)
	}
	if len(projected.ActionCandidates) != 3 {
		t.Fatalf("ActionCandidates = %d, want 3",
			len(projected.ActionCandidates))
	}
	if projected.ActionCandidates[1].ActionType != "cancel_backend" {
		t.Fatalf("second candidate = %q",
			projected.ActionCandidates[1].ActionType)
	}
}

func TestCaseActionFromQueuedActionIncludesLifecycle(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	cooldownUntil := now.Add(time.Hour)
	action := store.QueuedAction{
		ID:                 11,
		FindingID:          42,
		ActionType:         "analyze_table",
		ActionRisk:         "safe",
		Status:             "pending",
		ProposedAt:         now.Add(-time.Hour),
		ExpiresAt:          now.Add(24 * time.Hour),
		PolicyDecision:     "execute",
		Guardrails:         []string{"dedicated connection"},
		CooldownUntil:      &cooldownUntil,
		AttemptCount:       1,
		VerificationStatus: "not_started",
	}

	got := caseActionFromQueuedAction(action, now)

	if got.ID != "queue:11" {
		t.Fatalf("ID = %q, want queue:11", got.ID)
	}
	if got.Type != "analyze_table" {
		t.Fatalf("Type = %q, want analyze_table", got.Type)
	}
	if got.PolicyDecision != "execute" {
		t.Fatalf("PolicyDecision = %q, want execute", got.PolicyDecision)
	}
	if got.LifecycleState != store.ActionLifecycleBlocked {
		t.Fatalf("LifecycleState = %q, want blocked", got.LifecycleState)
	}
	if got.BlockedReason == "" {
		t.Fatalf("BlockedReason is empty")
	}
	if got.AttemptCount != 1 {
		t.Fatalf("AttemptCount = %d, want 1", got.AttemptCount)
	}
	if len(got.Guardrails) != 1 {
		t.Fatalf("Guardrails = %#v, want one guardrail", got.Guardrails)
	}
}

func TestCaseActionFromQueuedActionUsesQueueReasonFallback(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	action := store.QueuedAction{
		ID:             12,
		FindingID:      42,
		ActionType:     "create_index_concurrently",
		ActionRisk:     "moderate",
		Status:         "rejected",
		Reason:         "operator rejected risk",
		ProposedAt:     now.Add(-time.Hour),
		ExpiresAt:      now.Add(24 * time.Hour),
		PolicyDecision: "queue_for_approval",
	}

	got := caseActionFromQueuedAction(action, now)

	if got.ID != "queue:12" {
		t.Fatalf("ID = %q, want queue:12", got.ID)
	}
	if got.Status != "rejected" {
		t.Fatalf("Status = %q, want rejected", got.Status)
	}
	if got.LifecycleState != store.ActionLifecycleReady {
		t.Fatalf("LifecycleState = %q, want ready", got.LifecycleState)
	}
	if got.BlockedReason != "operator rejected risk" {
		t.Fatalf("BlockedReason = %q, want queue reason",
			got.BlockedReason)
	}
}

func TestEnrichCaseActionTimelineIncludesExpiredQueueLedger(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	findingID := insertActionHandlerFinding(t, pool, ctx, "expired_case_ledger")
	actionStore := store.NewActionStore(pool)
	expiresAt := time.Now().UTC().Add(-time.Hour)
	actionID, err := actionStore.ProposeWithMetadata(
		ctx, nil, findingID,
		"ANALYZE public.expired_case_ledger",
		"", "safe",
		store.ActionProposalMetadata{
			ActionType:         "analyze_table",
			PolicyDecision:     "execute",
			Guardrails:         []string{"dedicated connection"},
			VerificationStatus: "not_started",
			ShadowToilMinutes:  15,
			ExpiresAt:          &expiresAt,
		},
	)
	if err != nil {
		t.Fatalf("ProposeWithMetadata: %v", err)
	}
	if _, err := actionStore.MarkExpiredByReadiness(ctx); err != nil {
		t.Fatalf("MarkExpiredByReadiness: %v", err)
	}
	c := cases.Case{
		ID:        "case-expired-ledger",
		Title:     "expired queued action",
		SourceIDs: []string{strconv.Itoa(findingID)},
	}

	enrichCaseActionTimeline(ctx, &c, pool, actionStore)

	if len(c.Actions) != 1 {
		t.Fatalf("Actions len = %d, want one expired ledger action", len(c.Actions))
	}
	action := c.Actions[0]
	if action.ID != "queue:"+strconv.Itoa(actionID) {
		t.Fatalf("Action ID = %q, want queue:%d", action.ID, actionID)
	}
	if action.Status != "expired" {
		t.Fatalf("Status = %q, want expired", action.Status)
	}
	if action.LifecycleState != store.ActionLifecycleExpired {
		t.Fatalf("LifecycleState = %q, want expired", action.LifecycleState)
	}
	if action.BlockedReason != "action proposal expired" {
		t.Fatalf("BlockedReason = %q, want action proposal expired",
			action.BlockedReason)
	}

	report := cases.BuildShadowReport([]cases.Case{c})
	if report.Blocked != 1 || report.RequiresApproval != 1 {
		t.Fatalf("shadow counts = blocked %d approval %d, want 1/1",
			report.Blocked, report.RequiresApproval)
	}
	if len(report.Proof) != 1 || report.Proof[0].Status != "expired" {
		t.Fatalf("shadow proof = %#v, want expired status", report.Proof)
	}
	if len(report.BlockedReasons) != 1 ||
		report.BlockedReasons[0] != "action proposal expired" {
		t.Fatalf("BlockedReasons = %#v, want action proposal expired",
			report.BlockedReasons)
	}
}

func TestCaseActionFromActionLogIncludesOutcome(t *testing.T) {
	executedAt := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	measuredAt := executedAt.Add(2 * time.Minute)
	row := map[string]any{
		"id":           "88",
		"action_type":  "analyze",
		"outcome":      "success",
		"executed_at":  executedAt,
		"measured_at":  &measuredAt,
		"finding_id":   "42",
		"rollback_sql": "",
	}

	got := caseActionFromActionLog(row)

	if got.ID != "log:88" {
		t.Fatalf("ID = %q, want log:88", got.ID)
	}
	if got.Type != "analyze" {
		t.Fatalf("Type = %q, want analyze", got.Type)
	}
	if got.Status != "success" {
		t.Fatalf("Status = %q, want success", got.Status)
	}
	if got.LifecycleState != "executed" {
		t.Fatalf("LifecycleState = %q, want executed", got.LifecycleState)
	}
	if got.VerificationStatus != "verified" {
		t.Fatalf("VerificationStatus = %q, want verified",
			got.VerificationStatus)
	}
	if got.ProposedAt == nil || !got.ProposedAt.Equal(executedAt) {
		t.Fatalf("ProposedAt = %v, want executed_at", got.ProposedAt)
	}
}
