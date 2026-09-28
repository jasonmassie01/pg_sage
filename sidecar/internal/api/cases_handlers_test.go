package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
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

// caseGateExecutor is an executor with a real standing gate over an
// in-memory unattended policy (windows always open) and no emergency stop.
func caseGateExecutor(cfg *config.Config) *executor.Executor {
	e := executor.New(nil, cfg, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	e.WithEmergencyStopCheck(func(context.Context) bool { return false })
	e.SetExecutionMode("auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	e.EnableStandingPolicyDocument(doc, nil)
	return e
}

func caseAutonomousConfig() *config.Config {
	return &config.Config{
		Mode: "fleet",
		Trust: config.TrustConfig{
			Level: "autonomous", Tier3Safe: true, Tier3Moderate: true,
		},
	}
}

func registerCaseInstance(
	cfg *config.Config, exec *executor.Executor, status *fleet.InstanceStatus,
) *fleet.DatabaseManager {
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:     "prod",
		Config:   config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Executor: exec,
		Status:   status,
	})
	return mgr
}

func analyzeCase() cases.Case {
	return cases.Case{DatabaseName: "prod", ActionCandidates: []cases.ActionCandidate{{
		ActionType: "analyze_table", RiskTier: "safe",
	}}}
}

func TestEnrichCaseActionPoliciesUsesInstanceGate(t *testing.T) {
	cfg := caseAutonomousConfig()
	mgr := registerCaseInstance(cfg, caseGateExecutor(cfg), nil)
	c := analyzeCase()

	enrichCaseActionPolicies(&c, mgr, "prod")

	decision := c.ActionCandidates[0].PolicyDecision
	if decision == nil || decision.Decision != executor.PolicyDecisionExecute {
		t.Fatalf("policy decision = %#v, want execute", decision)
	}
	if len(c.ActionCandidates[0].Guardrails) == 0 {
		t.Fatalf("expected candidate guardrails")
	}
}

// A configured trust.maintenance_window that is closed blocks autonomous
// moderate actions without asking for approval.
func TestEnrichCaseActionPoliciesShowsAutoWindowBlock(t *testing.T) {
	cfg := caseAutonomousConfig()
	cfg.Trust.MaintenanceWindow = fmt.Sprintf("0 %d * * *", (time.Now().Hour()+2)%24)
	mgr := registerCaseInstance(cfg, caseGateExecutor(cfg), nil)
	c := cases.Case{DatabaseName: "prod", ActionCandidates: []cases.ActionCandidate{{
		ActionType: "create_index_concurrently", RiskTier: "moderate",
	}}}

	enrichCaseActionPolicies(&c, mgr, "prod")

	candidate := c.ActionCandidates[0]
	if candidate.PolicyDecision == nil || candidate.RequiresApproval ||
		!candidate.RequiresMaintenanceWindow ||
		candidate.BlockedReason != "outside_maintenance_window" {
		t.Fatalf("candidate = %#v, want auto window block without approval", candidate)
	}
}

func TestEnrichCaseActionPoliciesBlocksDisabledExecutor(t *testing.T) {
	cfg := caseAutonomousConfig()
	exec := caseGateExecutor(cfg)
	exec.SetExecutorEnabled(false)
	mgr := registerCaseInstance(cfg, exec, nil)
	c := analyzeCase()

	enrichCaseActionPolicies(&c, mgr, "prod")

	if got := c.ActionCandidates[0].BlockedReason; got != "executor_disabled" {
		t.Fatalf("BlockedReason = %q, want executor_disabled", got)
	}
}

func TestEnrichCaseActionPoliciesBlocksReplicaFromCapabilities(t *testing.T) {
	cfg := caseAutonomousConfig()
	mgr := registerCaseInstance(cfg, caseGateExecutor(cfg), &fleet.InstanceStatus{
		Platform: "postgres",
		Capabilities: fleet.ProviderCapabilities{
			Provider: "postgres", IsReplica: true,
		},
	})
	c := analyzeCase()

	enrichCaseActionPolicies(&c, mgr, "prod")

	if got := c.ActionCandidates[0].BlockedReason; got != "replica_mutation" {
		t.Fatalf("BlockedReason = %q, want replica_mutation", got)
	}
}

func TestEnrichCaseActionPoliciesFailsClosedWithoutExecutor(t *testing.T) {
	cfg := caseAutonomousConfig()
	mgr := registerCaseInstance(cfg, nil, nil)
	c := analyzeCase()
	c.ActionCandidates = append(c.ActionCandidates, cases.ActionCandidate{ActionType: "mystery"})

	enrichCaseActionPolicies(&c, mgr, "prod")

	if got := c.ActionCandidates[0].BlockedReason; got != "standing policy unavailable" {
		t.Fatalf("BlockedReason = %q, want standing policy unavailable", got)
	}
	if got := c.ActionCandidates[1].BlockedReason; got != "unknown action type" {
		t.Fatalf("unknown candidate BlockedReason = %q", got)
	}
	enrichCaseActionPolicies(&c, nil, "prod") // no manager: must not panic
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
		// SURF-12: "verified" requires a completed durable verification.
		"verification_verdict":      "success",
		"verification_completed_at": &measuredAt,
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

// TestQueryHintID_PreservesLargeQueryID is the D1 regression: a 64-bit
// pg_stat_statements queryid beyond 2^53 must round-trip exactly.
// The previous int64(floatValue(...)) path lost precision, so the
// ANY($1::bigint[]) lookup missed and the self-monitoring filter
// silently failed.
func TestQueryHintID_PreservesLargeQueryID(t *testing.T) {
	const big = int64(7656217072646174000) // > 2^53, typical 64-bit hash
	if int64(float64(big)) == big {
		t.Fatalf("test value %d does not exceed float64 precision; "+
			"choose a larger one", big)
	}
	row := map[string]any{"queryid": big}
	if got := queryHintID(row); got != big {
		t.Errorf("queryHintID = %d, want %d (precision lost)", got, big)
	}
}

// TestInt64Value_Types covers the integer and float branches and the
// default zero for unexpected types.
func TestInt64Value_Types(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"int64", int64(9007199254740993), 9007199254740993},
		{"int32", int32(42), 42},
		{"int", int(7), 7},
		{"float64 truncates", float64(3.9), 3},
		{"nil -> 0", nil, 0},
		{"string -> 0", "nope", 0},
	}
	for _, c := range cases {
		if got := int64Value(c.in); got != c.want {
			t.Errorf("%s: int64Value(%v) = %d, want %d",
				c.name, c.in, got, c.want)
		}
	}
}
