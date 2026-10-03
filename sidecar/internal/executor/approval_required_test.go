package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// No concurrent access tests: findingRequest builds a fresh request from
// its arguments.

func hasApprovalGuardrail(req policy.ActionRequest) bool {
	if req.Contract == nil {
		return false
	}
	for _, g := range req.Contract.Guardrails {
		if g == policy.GuardrailApprovalRequired {
			return true
		}
	}
	return false
}

func TestFindingRequestApprovalRequirement(t *testing.T) {
	drop := analyzer.Finding{Category: "unused_index", ObjectIdentifier: "public.idx_a",
		RecommendedSQL: `DROP INDEX CONCURRENTLY "public"."idx_a"`}
	replicaDrop := drop
	replicaDrop.Detail = map[string]any{
		analyzer.DetailApprovalRequired: "2 streaming replicas; standby index usage unknown"}
	emptyMarker := drop
	emptyMarker.Detail = map[string]any{analyzer.DetailApprovalRequired: ""}
	cases := []struct {
		name string
		f    analyzer.Finding
		want bool
	}{
		{"plain drop", drop, false},
		{"drop with replicas", replicaDrop, true},
		{"empty marker", emptyMarker, false},
		{"reload-only GUC", analyzer.Finding{
			RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"}, false},
		{"restart GUC", analyzer.Finding{
			RecommendedSQL: "ALTER SYSTEM SET shared_buffers = '2GB'"}, true},
		{"restart GUC pre-PG18", analyzer.Finding{
			RecommendedSQL: "ALTER SYSTEM SET autovacuum_max_workers = 5"}, true},
		// Executable but not on the advisor's autonomous list.
		{"non-autonomous GUC", analyzer.Finding{
			RecommendedSQL: "ALTER SYSTEM SET jit = off"}, true},
		{"allowlisted reloption", analyzer.Finding{ObjectIdentifier: "public.t",
			RecommendedSQL: "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.02)"},
			false},
	}
	for _, c := range cases {
		req := findingRequest(c.f, false)
		if req.Contract == nil {
			t.Fatalf("%s: no contract for %q", c.name, c.f.RecommendedSQL)
		}
		if got := hasApprovalGuardrail(req); got != c.want {
			t.Errorf("%s: approval guardrail = %v, want %v", c.name, got, c.want)
		}
	}
}

// The marker survives the durable recommendation round trip (Evidence is
// JSON), where a non-string value must not grant autonomy or panic.
func TestApprovalRequiredReasonTypes(t *testing.T) {
	cases := []struct {
		detail map[string]any
		want   bool
	}{
		{nil, false},
		{map[string]any{}, false},
		{map[string]any{analyzer.DetailApprovalRequired: "why"}, true},
		{map[string]any{analyzer.DetailApprovalRequired: true}, true},
		{map[string]any{analyzer.DetailApprovalRequired: 7}, true},
		{map[string]any{analyzer.DetailApprovalRequired: false}, false},
	}
	for _, c := range cases {
		f := analyzer.Finding{Detail: c.detail, RecommendedSQL: "DROP INDEX public.i"}
		if got := approvalRequiredReason(f) != ""; got != c.want {
			t.Errorf("approvalRequiredReason(%v) set = %v, want %v", c.detail, got, c.want)
		}
	}
}
