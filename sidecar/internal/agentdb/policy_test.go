package agentdb

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDecideRequestCoversIsolationPolicy(t *testing.T) {
	tests := []struct {
		name     string
		req      RequestCreate
		decision string
		status   string
	}{
		{
			name: "schema requests are approved when identity is complete",
			req: RequestCreate{
				TenantID:       "tenant_a",
				AgentID:        "agent_a",
				IsolationType:  "schema",
				IdempotencyKey: "idem-a",
				BackupRequired: true,
			},
			decision: "allow",
			status:   "approved",
		},
		{
			name: "external databases require human review",
			req: RequestCreate{
				TenantID:       "tenant_a",
				AgentID:        "agent_a",
				IsolationType:  "external",
				IdempotencyKey: "idem-b",
			},
			decision: "review",
			status:   "requested",
		},
		{
			name: "local database mode is approved",
			req: RequestCreate{
				TenantID:       "tenant_a",
				AgentID:        "agent_a",
				IsolationType:  "database",
				IdempotencyKey: "idem-c",
			},
			decision: "allow",
			status:   "approved",
		},
		{
			name: "instance mode is approved for cloud plan creation",
			req: RequestCreate{
				TenantID:       "tenant_a",
				AgentID:        "agent_a",
				IsolationType:  "instance",
				IdempotencyKey: "idem-instance",
			},
			decision: "allow",
			status:   "approved",
		},
		{
			name: "branch mode is deferred from this pg_sage slice",
			req: RequestCreate{
				TenantID:       "tenant_a",
				AgentID:        "agent_a",
				IsolationType:  "branch",
				IdempotencyKey: "idem-d",
			},
			decision: "defer",
			status:   "deferred",
		},
		{
			name: "missing agent identity is denied",
			req: RequestCreate{
				TenantID:       "tenant_a",
				IsolationType:  "schema",
				IdempotencyKey: "idem-e",
			},
			decision: "deny",
			status:   "denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideRequest(tt.req)
			if got.Decision != tt.decision {
				t.Fatalf("decision = %q, want %q", got.Decision, tt.decision)
			}
			if got.Status != tt.status {
				t.Fatalf("status = %q, want %q", got.Status, tt.status)
			}
			if len(got.Reasons) == 0 {
				t.Fatal("expected at least one policy reason")
			}
		})
	}
}

func TestDecideRequestEnterprisePolicyGates(t *testing.T) {
	tests := []struct {
		name          string
		req           RequestCreate
		wantDecision  string
		wantStatus    string
		wantReasonSub string
	}{
		{
			name: "restricted data without masking is denied",
			req: RequestCreate{
				TenantID:           "tenant_a",
				AgentID:            "agent_a",
				IsolationType:      "schema",
				DataClassification: "restricted",
			},
			wantDecision:  "deny",
			wantStatus:    "denied",
			wantReasonSub: "masking policy",
		},
		{
			name: "restricted data with masking needs review",
			req: RequestCreate{
				TenantID:           "tenant_a",
				AgentID:            "agent_a",
				IsolationType:      "schema",
				DataClassification: "pii",
				MaskingPolicyID:    "mask_agent_pii",
				ApprovalSLASeconds: 3600,
			},
			wantDecision:  "review",
			wantStatus:    "requested",
			wantReasonSub: "sensitive data",
		},
		{
			name: "region outside allowed list is denied",
			req: RequestCreate{
				TenantID:       "tenant_a",
				AgentID:        "agent_a",
				IsolationType:  "instance",
				Provider:       ProviderGCPCloudSQL,
				Region:         "europe-west1",
				AllowedRegions: []string{"us-central1", "us-east1"},
				BudgetUSD:      25,
			},
			wantDecision:  "deny",
			wantStatus:    "denied",
			wantReasonSub: "region europe-west1",
		},
		{
			name: "cloud instance without budget requires review",
			req: RequestCreate{
				TenantID:      "tenant_a",
				AgentID:       "agent_a",
				IsolationType: "instance",
				Provider:      ProviderAWSRDS,
			},
			wantDecision:  "review",
			wantStatus:    "requested",
			wantReasonSub: "budget",
		},
		{
			name: "cloud instance with budget and allowed region is approved",
			req: RequestCreate{
				TenantID:       "tenant_a",
				AgentID:        "agent_a",
				IsolationType:  "instance",
				Provider:       ProviderAWSRDS,
				Region:         "us-east-1",
				AllowedRegions: []string{"us-east-1"},
				BudgetUSD:      100,
			},
			wantDecision:  "allow",
			wantStatus:    "approved",
			wantReasonSub: "cloud instance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideRequest(tt.req)
			if got.Decision != tt.wantDecision || got.Status != tt.wantStatus {
				t.Fatalf("decision/status = %s/%s, want %s/%s reasons=%v",
					got.Decision, got.Status, tt.wantDecision, tt.wantStatus, got.Reasons)
			}
			if !reasonsContain(got.Reasons, tt.wantReasonSub) {
				t.Fatalf("reasons = %#v, want substring %q", got.Reasons, tt.wantReasonSub)
			}
		})
	}
}

func TestBuildTuningHintsIncludesSpecializedPacks(t *testing.T) {
	hints := BuildTuningHints(TuningContext{
		WorkloadTypes: []string{"vector", "postgis", "jsonb"},
		Extensions:    []string{"pgvector", "postgis", "pg_stat_statements"},
	})

	wantKinds := map[string]bool{
		"vector":    false,
		"postgis":   false,
		"jsonb":     false,
		"extension": false,
	}
	for _, hint := range hints {
		if _, ok := wantKinds[hint.Kind]; ok {
			wantKinds[hint.Kind] = true
		}
		if hint.Title == "" || hint.Detail == "" {
			t.Fatalf("hint has empty title/detail: %#v", hint)
		}
	}
	for kind, found := range wantKinds {
		if !found {
			t.Fatalf("missing %s tuning hint in %#v", kind, hints)
		}
	}
}

func TestBudgetStatusTransitions(t *testing.T) {
	dep := Deployment{BudgetUSD: 10}

	under := BudgetStatus(dep, CostSummary{TotalUSD: 7})
	if under.State != "under_budget" || under.Action != "none" {
		t.Fatalf("under budget = %#v", under)
	}

	soft := BudgetStatus(dep, CostSummary{TotalUSD: 9.25})
	if soft.State != "soft_limit" || soft.Action != "warn" {
		t.Fatalf("soft budget = %#v", soft)
	}

	hard := BudgetStatus(dep, CostSummary{TotalUSD: 10.01})
	if hard.State != "hard_limit" || hard.Action != "pause" {
		t.Fatalf("hard budget = %#v", hard)
	}
}

func TestCleanupDecisionRequiresArchiveAndVerifiedBackup(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Hour)
	dep := Deployment{
		Status:         "active",
		BackupRequired: true,
		LeaseExpiresAt: &expiredAt,
	}

	active := CleanupDecisionFor(dep, nil, now)
	if active.Action != "archive" || active.CanDelete {
		t.Fatalf("active cleanup = %#v", active)
	}

	dep.Status = "archived"
	withoutBackup := CleanupDecisionFor(dep, nil, now)
	if withoutBackup.Action != "wait_for_verified_backup" || withoutBackup.CanDelete {
		t.Fatalf("archived without backup cleanup = %#v", withoutBackup)
	}

	withBackup := CleanupDecisionFor(dep, []Backup{{
		Status:            "restore_verified",
		RestoreVerifiedAt: &now,
	}}, now)
	if withBackup.Action != "delete_ready" || !withBackup.CanDelete {
		t.Fatalf("archived with backup cleanup = %#v", withBackup)
	}
}

func TestDeleteGuardErrorIsDistinguishable(t *testing.T) {
	if !errors.Is(ErrRestoreRequired, ErrInvalid) {
		t.Fatal("ErrRestoreRequired should remain distinguishable and invalid")
	}
}

func reasonsContain(reasons []string, sub string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, sub) {
			return true
		}
	}
	return false
}
