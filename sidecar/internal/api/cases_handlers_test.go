package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
