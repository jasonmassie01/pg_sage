package executor

import (
	"errors"
	"strings"
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
	// The drops carry a passed clone rehearsal, so the ladder's entry rule
	// (lab-03a) does not decide them; "unrehearsed drop" pins that rule.
	rehearsed := ladderDetail(t, passedClone())
	drop := analyzer.Finding{Category: "unused_index", ObjectIdentifier: "public.idx_a",
		RecommendedSQL: `DROP INDEX CONCURRENTLY "public"."idx_a"`,
		Detail:         map[string]any{"ladder": rehearsed}}
	unrehearsed := analyzer.Finding{Category: "unused_index",
		ObjectIdentifier: "public.idx_a", RecommendedSQL: drop.RecommendedSQL}
	replicaDrop := drop
	replicaDrop.Detail = map[string]any{"ladder": rehearsed,
		analyzer.DetailApprovalRequired: "2 streaming replicas; standby index usage unknown"}
	emptyMarker := drop
	emptyMarker.Detail = map[string]any{"ladder": rehearsed,
		analyzer.DetailApprovalRequired: ""}
	cases := []struct {
		name string
		f    analyzer.Finding
		want bool
	}{
		{"plain drop", drop, false},
		{"unrehearsed drop", unrehearsed, true},
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

// advisors-07: idle_in_transaction_session_timeout is approval-only at
// database scope and refused under ALTER SYSTEM. An ALTER DATABASE of a
// setting off the autonomous list needs an operator as ALTER SYSTEM does.
func TestTimeoutScope_DatabaseApprovalRequired(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{`ALTER DATABASE "app" SET idle_in_transaction_session_timeout = '5min'`, true},
		{`ALTER DATABASE app SET idle_in_transaction_session_timeout TO '10min';`, true},
		{`ALTER DATABASE "app" RESET idle_in_transaction_session_timeout`, true},
		{`ALTER SYSTEM SET idle_in_transaction_session_timeout = '5min'`, true},
		// Executable but not autonomous at any scope.
		{`ALTER DATABASE "app" SET jit = off`, true},
		// Autonomous settings keep running unattended at database scope.
		{`ALTER DATABASE "app" SET work_mem = '64MB'`, false},
		{`ALTER DATABASE "app" SET random_page_cost = 1.1`, false},
	}
	for _, c := range cases {
		f := analyzer.Finding{Category: "connection_tuning", ObjectIdentifier: "instance",
			RecommendedSQL: c.sql}
		if got := approvalRequiredReason(f) != ""; got != c.want {
			t.Errorf("approvalRequiredReason(%q) set = %v, want %v (%q)", c.sql, got,
				c.want, approvalRequiredReason(f))
		}
	}
}

func TestTimeoutScope_RefusedUnderAlterSystem(t *testing.T) {
	err := ValidateExecutorSQL("ALTER SYSTEM SET idle_in_transaction_session_timeout = '5min'")
	if !errors.Is(err, ErrDisallowedSQL) || !strings.Contains(err.Error(), "ALTER DATABASE") {
		t.Fatalf("ALTER SYSTEM of a database-scope timeout: err = %v, want a refusal "+
			"naming ALTER DATABASE", err)
	}
	if err := ValidateExecutorSQL(
		"ALTER SYSTEM RESET idle_in_transaction_session_timeout"); !errors.Is(err,
		ErrDisallowedSQL) {
		t.Fatalf("ALTER SYSTEM RESET of a database-scope timeout: err = %v", err)
	}
	for _, sql := range []string{
		`ALTER DATABASE "app" SET idle_in_transaction_session_timeout = '5min'`,
		`ALTER DATABASE "app" RESET idle_in_transaction_session_timeout`,
	} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want allowed", sql, err)
		}
	}
	for _, sql := range []string{
		`ALTER DATABASE "app" SET statement_timeout = '30s'`,
		`ALTER SYSTEM SET statement_timeout = '30s'`,
		`ALTER DATABASE "app" SET idle_session_timeout = '1h'`,
	} {
		if err := ValidateExecutorSQL(sql); !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want refused (advisory only)", sql, err)
		}
	}
}

// The executor never runs the timeout unattended: its request carries the
// approval guardrail whichever producer marked it, or none did.
func TestTimeoutScope_QueuedForApproval(t *testing.T) {
	f := analyzer.Finding{Category: "connection_tuning", ObjectIdentifier: "instance",
		Severity: "info",
		RecommendedSQL: `ALTER DATABASE "app" SET idle_in_transaction_session_timeout = ` +
			`'5min'`}
	req := findingRequest(f, false)
	if req.Contract == nil {
		t.Fatalf("no contract for %q", f.RecommendedSQL)
	}
	if !hasApprovalGuardrail(req) {
		t.Errorf("database-scope timeout must carry the approval guardrail: %+v",
			req.Contract.Guardrails)
	}
}
