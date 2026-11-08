package executor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// No concurrent access tests: FindingActionPreview is a pure function of
// its arguments and reads no shared state.

func TestFindingActionPreviewAnalyzeIsSafeWithoutApproval(t *testing.T) {
	p := FindingActionPreview("ANALYZE public.orders", nil)
	if p.ActionType != "analyze_table" || p.ContractTier != "safe" {
		t.Fatalf("ANALYZE preview = %+v, want analyze_table/safe", p)
	}
	if p.ApprovalRequired || p.ApprovalReason != "" {
		t.Fatalf("ANALYZE must not need approval: %+v", p)
	}
}

func TestFindingActionPreviewRestartGUCNeedsApproval(t *testing.T) {
	p := FindingActionPreview("ALTER SYSTEM SET shared_buffers = '2GB'", nil)
	if p.ActionType != "alter_system_guc" || p.ContractTier != "moderate" {
		t.Fatalf("shared_buffers preview = %+v, want alter_system_guc/moderate", p)
	}
	if !p.ApprovalRequired || !strings.Contains(p.ApprovalReason, "restart") {
		t.Fatalf("shared_buffers must need approval for a restart: %+v", p)
	}
}

func TestFindingActionPreviewEmptySQLHasNoContract(t *testing.T) {
	for _, sql := range []string{"", "   \n\t"} {
		p := FindingActionPreview(sql, map[string]any{})
		if p.ActionType != "" || p.ContractTier != ContractTierNone {
			t.Fatalf("preview(%q) = %+v, want no action and tier none", sql, p)
		}
		if p.ApprovalRequired || p.ApprovalReason != "" {
			t.Fatalf("preview(%q) must not need approval: %+v", sql, p)
		}
	}
}

func TestFindingActionPreviewUnclassifiedSQLHasNoContract(t *testing.T) {
	// Invalid input: SQL the executor has no contract for.
	for _, sql := range []string{"SELECT 1", "DROP TABLE public.orders", "VACUUM FULL t"} {
		p := FindingActionPreview(sql, nil)
		if p.ActionType != "" || p.ContractTier != ContractTierNone {
			t.Fatalf("preview(%q) = %+v, want tier none", sql, p)
		}
	}
}

func TestFindingActionPreviewDropIndexUsesContractTier(t *testing.T) {
	p := FindingActionPreview(`DROP INDEX CONCURRENTLY "public"."ix_sub"`, nil)
	if p.ActionType != "drop_unused_index" || p.ContractTier != "moderate" {
		t.Fatalf("drop preview = %+v, want drop_unused_index/moderate", p)
	}
}

func TestFindingActionPreviewProducerApprovalReason(t *testing.T) {
	detail := map[string]any{
		analyzer.DetailApprovalRequired: "standby index usage is unknown",
	}
	p := FindingActionPreview(`DROP INDEX CONCURRENTLY public.ix`, detail)
	if !p.ApprovalRequired || p.ApprovalReason != "standby index usage is unknown" {
		t.Fatalf("producer approval reason lost: %+v", p)
	}
	p = FindingActionPreview("ANALYZE t",
		map[string]any{analyzer.DetailApprovalRequired: true})
	if !p.ApprovalRequired || p.ApprovalReason == "" {
		t.Fatalf("approval_required=true must need approval: %+v", p)
	}
	p = FindingActionPreview("ANALYZE t",
		map[string]any{analyzer.DetailApprovalRequired: false})
	if p.ApprovalRequired {
		t.Fatalf("approval_required=false must not need approval: %+v", p)
	}
}
