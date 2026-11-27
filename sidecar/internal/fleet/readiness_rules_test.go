package fleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	prov "github.com/pg-sage/sidecar/internal/provider"
)

// readyCaps is a database that has everything auto-safe needs.
func readyCaps(analyze string) ProviderCapabilities {
	caps := ProviderCapabilities{Provider: "postgres",
		Permissions: map[string]CapabilityStatus{"analyze": {Status: analyze},
			"read_stats": {Status: "ok"}},
		Extensions: map[string]string{"pg_stat_statements": "available"},
		ActionFamilies: []ActionFamilyReadiness{{ActionType: "analyze_table",
			Supported: true, Decision: executor.PolicyDecisionExecute}}}
	caps.Blockers = readinessBlockers(caps)
	caps.ReadyForAutoSafe = readyForAutoSafe(caps)
	return caps
}

// Only an unknown ANALYZE permission blocks: "limited" means it is checked
// per table at execution.
func TestReadinessBlocksOnlyOnUnknownAnalyze(t *testing.T) {
	for _, tc := range []struct {
		status  string
		blocked bool
	}{{"ok", false}, {"limited", false}, {"unknown", true}, {"", true}} {
		caps := readyCaps(tc.status)
		blocked := containsString(caps.Blockers, "ANALYZE permission unknown")
		if blocked != tc.blocked || caps.ReadyForAutoSafe == tc.blocked {
			t.Errorf("analyze %q: blockers %v ready %v, want blocked=%v", tc.status,
				caps.Blockers, caps.ReadyForAutoSafe, tc.blocked)
		}
	}
}

// Must not change: auto-safe still needs pg_stat_statements and an
// executable analyze_table family.
func TestReadinessStillRequiresStatementsAndExecutableAnalyze(t *testing.T) {
	caps := readyCaps("ok")
	caps.Extensions["pg_stat_statements"] = "not_installed"
	caps.Blockers = readinessBlockers(caps)
	if readyForAutoSafe(caps) ||
		!containsString(caps.Blockers, "pg_stat_statements not_installed") {
		t.Fatalf("missing pg_stat_statements: ready, blockers %v", caps.Blockers)
	}
	caps = readyCaps("ok")
	caps.ActionFamilies[0].Decision = executor.PolicyDecisionQueueApproval
	caps.Blockers = readinessBlockers(caps)
	if readyForAutoSafe(caps) {
		t.Fatal("analyze_table needing approval still reported auto-safe ready")
	}
	caps.ActionFamilies = nil
	if readyForAutoSafe(caps) {
		t.Fatal("no analyze_table family still reported auto-safe ready")
	}
}

func TestSummarizeReadinessCountsLimitedAnalyzeAsReady(t *testing.T) {
	got := SummarizeReadiness([]DatabaseStatus{
		{Name: "a", Status: &InstanceStatus{Capabilities: readyCaps("limited")}},
		{Name: "b", Status: &InstanceStatus{Capabilities: readyCaps("unknown")}},
	})
	if got.ReadyForAutoSafe != 1 || got.Unknown != 1 || got.Blocked != 0 {
		t.Fatalf("summary = %+v, want 1 ready and 1 unknown", got)
	}
}

// A closed pool fails every probe: nothing is claimed and ANALYZE blocks.
func TestClosedPoolLeavesAnalyzeUnknown(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	pool.Close()
	caps := CollectProviderCapabilities(t.Context(), pool, "postgres", executeAnalyze)
	if caps.Permissions["analyze"].Status != "unknown" || caps.ReadyForAutoSafe {
		t.Fatalf("closed pool analyze = %#v ready=%v", caps.Permissions["analyze"],
			caps.ReadyForAutoSafe)
	}
	if !containsString(caps.Blockers, "ANALYZE permission unknown") {
		t.Fatalf("closed pool blockers = %v", caps.Blockers)
	}
	if !containsString(caps.Limitations, maintainProbeFailed) {
		t.Fatalf("closed pool limitations = %v, want the maintenance probe failure",
			caps.Limitations)
	}
}

// With no pool the adapter's enable paths stay visible as hints, while the
// runtime status claims nothing.
func TestNilPoolKeepsHintsWithoutFabricatedEvidence(t *testing.T) {
	caps := CollectProviderCapabilities(context.Background(), nil, "azure", executeAnalyze)
	const allowlist = "azure_extensions_allowlist_required"
	for _, ext := range []string{"hypopg", "pg_hint_plan", "auto_explain"} {
		if caps.ExtensionHints[ext] != allowlist {
			t.Errorf("hint %s = %q, want %q", ext, caps.ExtensionHints[ext], allowlist)
		}
		if caps.Extensions[ext] != "unknown" {
			t.Errorf("runtime %s = %q without evidence", ext, caps.Extensions[ext])
		}
	}
	for name, p := range caps.Permissions {
		if p.Status == "ok" || p.Status == "limited" {
			t.Errorf("permission %s = %q without a probe", name, p.Status)
		}
	}
	for ext, hint := range caps.ExtensionHints {
		if hint == "unknown" || hint == "" {
			t.Errorf("hint %s carries no information: %q", ext, hint)
		}
	}
	if caps.ReadyForAutoSafe {
		t.Fatal("nil pool reported auto-safe ready")
	}
}

func TestExtensionHintsAreCopiesPerProvider(t *testing.T) {
	a := BuildProviderCapabilities("cloud-sql", false, executeAnalyze)
	if a.ExtensionHints["pg_hint_plan"] != "provider_parameter_required" {
		t.Fatalf("cloud-sql hint = %q", a.ExtensionHints["pg_hint_plan"])
	}
	a.ExtensionHints["pg_hint_plan"] = "changed"
	b := BuildProviderCapabilities("cloud-sql", false, executeAnalyze)
	if b.ExtensionHints["pg_hint_plan"] != "provider_parameter_required" {
		t.Fatal("hints share a map across capability builds")
	}
	if self := BuildProviderCapabilities("postgres", false, executeAnalyze); len(
		self.ExtensionHints) != 0 {
		t.Fatalf("self-managed has hints %v, want none (every value is unknown)",
			self.ExtensionHints)
	}
}

// CHECK-02: no production readiness claims a schema CREATE probe it lacks.
func TestNoCreateSchemaObjectPermission(t *testing.T) {
	caps := CollectProviderCapabilities(t.Context(), nil, "postgres", executeAnalyze)
	if _, ok := caps.Permissions["create_schema_object"]; ok {
		t.Fatal("create_schema_object is reported without a probe")
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "create_schema_object") {
			t.Errorf("%s still names create_schema_object", file)
		}
	}
}

// Readiness and the gate agree on provider support: for every provider and
// family, readiness says "unsupported by provider" exactly when the gate
// blocks it as provider_unsupported.
func TestReadinessProviderSupportMatchesGate(t *testing.T) {
	families := buildActionFamilyReadiness(t.Context(), ProviderCapabilities{Provider: "postgres"},
		executeAnalyze)
	if f := actionReadiness(t, families, "alter_system_guc"); !f.Supported {
		t.Fatalf("self-managed alter_system_guc = %#v, want supported", f)
	}
	for _, profile := range prov.Profiles() {
		adapter := AdapterForProvider(profile.Name)
		for _, actionType := range executor.ContractActionTypes() {
			contract, _ := executor.ContractForActionType(actionType)
			gateBlocks := gateBlocksProvider(t, profile.Name, contract)
			if adapter.SupportsAction(actionType) == gateBlocks {
				t.Errorf("%s/%s: readiness supported=%v, gate provider-blocks=%v",
					profile.Name, actionType, adapter.SupportsAction(actionType), gateBlocks)
			}
		}
	}
}

func gateBlocksProvider(t *testing.T, provider string, c executor.ActionContract) bool {
	t.Helper()
	gate := policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{ExecutorEnabled: true, Provider: provider}, nil
		},
	})
	explainer, ok := gate.(policy.Explainer)
	if !ok {
		t.Fatal("standing gate cannot explain")
	}
	decision := explainer.Explain(t.Context(), policy.ActionRequest{ExplainFamily: true,
		Contract: &policy.ActionContract{ActionType: c.ActionType,
			RiskTier: policy.RiskSafe, ProviderSupport: c.ProviderSupport}})
	return decision.Reason == policy.ReasonProviderUnsupported
}
