package decide

import (
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// §6.2.6: every existing MCP tool is mapped to a class, so a new tool
// fails this census until someone decides how agents may use it.
func TestEveryMCPToolIsClassified(t *testing.T) {
	var missing []string
	for _, tool := range mcp.NewServer(nil).Tools() {
		if _, known := toolClasses[tool.Name]; !known {
			missing = append(missing, tool.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("MCP tools without a §6.2.6 class (add them to toolClasses): %v", missing)
	}
	// And no stale entries: every classified tool exists.
	names := map[string]bool{}
	for _, tool := range mcp.NewServer(nil).Tools() {
		names[tool.Name] = true
	}
	for name := range toolClasses {
		if !names[name] && !agentOnlyTools[name] {
			t.Fatalf("toolClasses lists %q, which is not an MCP tool", name)
		}
	}
}

func TestToolClassTable(t *testing.T) {
	cases := []struct {
		tool string
		kind agentguard.ToolKind
		cap  Capability
	}{
		{"optimize_query", agentguard.ToolPropose, CapDDLAdditive},
		{"ensure_fk_indexes", agentguard.ToolPropose, CapDDLAdditive},
		{"sre_propose_action", agentguard.ToolPropose, CapMaint},
		{"sre_request_execution", agentguard.ToolPropose, CapMaint},
		{"specialist_request_remediation", agentguard.ToolPropose, CapMaint},
		{"set_maintenance_policy", agentguard.ToolPropose, CapPolicyProposal},
		{"propose_policy_change", agentguard.ToolPropose, CapPolicyProposal},
		{"sre_draft_runbook", agentguard.ToolPropose, CapPolicyProposal},
		{"sre_compile_runbook", agentguard.ToolPropose, CapPolicyProposal},
		{"propose_fact", agentguard.ToolPropose, ""},
		{"mark_object", agentguard.ToolPropose, ""},
		{"report_source_fix", agentguard.ToolPropose, ""},
		{"sre_evaluate_autonomy", agentguard.ToolPropose, ""},
		{"top_queries", agentguard.ToolRead, ""},
		{"get_policy", agentguard.ToolRead, ""},
		{"decide_fact", KindApprove, ""},
		{"declare_table_contract", KindApprove, ""},
		{"register_consumer", KindApprove, ""},
		{"sre_downgrade_autonomy", KindApprove, ""},
		{"sre_review_investigation", KindApprove, ""},
		{"agent_query", agentguard.ToolAgent, CapRead},
	}
	for _, tc := range cases {
		kind, cap := KindFor(tc.tool), CapabilityFor(tc.tool, "")
		if kind != tc.kind || cap != tc.cap {
			t.Fatalf("%s = (%s, %q), want (%s, %q)", tc.tool, kind, cap, tc.kind, tc.cap)
		}
	}
}

func TestUnknownToolFailsClosed(t *testing.T) {
	if got := KindFor("brand_new_tool"); got != agentguard.ToolAgent {
		t.Fatalf("KindFor(unknown) = %s, want agent (every D-step applies)", got)
	}
	if got := KindFor(""); got != agentguard.ToolAgent {
		t.Fatalf("KindFor(\"\") = %s, want agent", got)
	}
	if got := CapabilityFor("brand_new_tool", "CREATE INDEX x ON t (a)"); got != "" {
		t.Fatalf("CapabilityFor(unknown) = %q, want none (D4 denies)", got)
	}
}

func TestSchemaIntentsClassifyTheirSQL(t *testing.T) {
	cases := map[string]Capability{
		"":                                    CapDDLDestructive, // unknown: worst case
		"CREATE TABLE app.t (id int)":         CapDDLAdditive,
		"ALTER TABLE app.t ADD COLUMN c text": CapDDLAdditive,
		"CREATE INDEX CONCURRENTLY i ON app.t (c)":            CapDDLAdditive,
		"CREATE INDEX i ON app.t (c)":                         CapDDLLocking,
		"ALTER TABLE app.t ALTER COLUMN c SET NOT NULL":       CapDDLLocking,
		"ALTER TABLE app.t ALTER COLUMN c TYPE bigint":        CapDDLLocking,
		"DROP TABLE app.t":                                    CapDDLDestructive,
		"ALTER TABLE app.t DROP COLUMN c":                     CapDDLDestructive,
		"TRUNCATE app.t":                                      CapDDLDestructive,
		"drop index app.i":                                    CapDDLDestructive,
		"ALTER TABLE app.t DROP CONSTRAINT k":                 CapDDLDestructive,
		"DROP SCHEMA app CASCADE":                             CapDDLDestructive,
		"CREATE TABLE a (id int); DROP TABLE b":               CapDDLDestructive,
		"-- DROP TABLE x\nCREATE TABLE app.t (id int)":        CapDDLAdditive,
	}
	for sql, want := range cases {
		for _, tool := range []string{"apply_migration", "request_change"} {
			if got := CapabilityFor(tool, sql); got != want {
				t.Fatalf("CapabilityFor(%s, %q) = %q, want %q", tool, sql, got, want)
			}
		}
	}
}
