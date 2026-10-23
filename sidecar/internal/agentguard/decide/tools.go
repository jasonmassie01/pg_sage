package decide

import (
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/migration"
)

// KindApprove marks the approve-scope tools: a person's decisions, which
// an agent never reaches (MCP refuses them with -32005 first).
const KindApprove agentguard.ToolKind = "approve"

type toolClass struct {
	kind agentguard.ToolKind
	cap  Capability
	// ddlFromSQL classes the request from its SQL (schema intents).
	ddlFromSQL bool
}

func read() toolClass                  { return toolClass{kind: agentguard.ToolRead} }
func propose(c Capability) toolClass   { return toolClass{kind: agentguard.ToolPropose, cap: c} }
func approve() toolClass               { return toolClass{kind: KindApprove} }
func agentTool(c Capability) toolClass { return toolClass{kind: agentguard.ToolAgent, cap: c} }

// toolClasses is §6.2.6: every MCP tool and its class. A tool missing here
// fails TestEveryMCPToolIsClassified.
var toolClasses = map[string]toolClass{
	// Schema intents: ddl_* from the classifier, L2 until G3.
	"apply_migration":   {kind: agentguard.ToolPropose, ddlFromSQL: true},
	"request_change":    {kind: agentguard.ToolPropose, ddlFromSQL: true},
	"optimize_query":    propose(CapDDLAdditive),
	"ensure_fk_indexes": propose(CapDDLAdditive),
	// pg_sage's own maintenance candidates, attributed to the agent.
	"sre_propose_action":             propose(CapMaint),
	"sre_request_execution":          propose(CapMaint),
	"specialist_request_remediation": propose(CapMaint),
	// Policy proposals: always a person, two when widening.
	"set_maintenance_policy": propose(CapPolicyProposal),
	"propose_policy_change":  propose(CapPolicyProposal),
	"sre_draft_runbook":      propose(CapPolicyProposal),
	"sre_compile_runbook":    propose(CapPolicyProposal),
	// Unchanged: facts stay proposed until a person confirms.
	"propose_fact": propose(""), "mark_object": propose(""),
	"report_source_fix": propose(""), "sre_evaluate_autonomy": propose(""),
	// Approve scope: out of an agent's reach.
	"decide_fact": approve(), "declare_table_contract": approve(),
	"register_consumer": approve(), "sre_downgrade_autonomy": approve(),
	"sre_review_investigation": approve(),
	// Read tools.
	"ask_sage": read(), "explain_query": read(), "fleet_findings": read(),
	"get_guarantee_status": read(), "get_ledger": read(), "get_policy": read(),
	"get_source_fix_packet": read(), "get_value": read(), "lint_migration": read(),
	"list_databases": read(), "list_facts": read(), "query_sources": read(),
	"specialist_investigation_result": read(), "specialist_investigation_status": read(),
	"specialist_investigation_transcript": read(),
	"specialist_open_investigation":       read(), "sre_get_autonomy": read(),
	"sre_get_evidence": read(), "sre_get_investigation": read(),
	"sre_get_runbook": read(), "sre_get_slo": read(), "sre_get_transcript": read(),
	"sre_list_changes":   read(),
	"sre_list_incidents": read(), "sre_list_runbooks": read(), "sre_list_slos": read(),
	"sre_runbook_runs": read(), "sre_similar_incidents": read(), "top_queries": read(),
	"whatif_index": read(),
	// agent_* tools (G1+): every D-step applies.
	"agent_query": agentTool(CapRead), "agent_whoami": agentTool(CapRead),
	// The capability class of a grant request is the class it asks for;
	// G1 grants read only.
	"agent_request_capability": agentTool(CapRead),
}

// agentOnlyTools are classified before their MCP tool ships.
var agentOnlyTools = map[string]bool{"agent_query": true, "agent_whoami": true}

// KindFor is the tool kind of an MCP tool. An unknown tool (or none) gets
// ToolAgent, so every D-step applies to it.
func KindFor(tool string) agentguard.ToolKind {
	if c, ok := toolClasses[tool]; ok {
		return c.kind
	}
	return agentguard.ToolAgent
}

// CapabilityFor is the capability class of a request from an MCP tool:
// the table's class, or for a schema intent the class of its SQL. An
// unknown tool has none, so D4 denies it.
func CapabilityFor(tool, sql string) Capability {
	c, ok := toolClasses[tool]
	switch {
	case !ok:
		return ""
	case c.ddlFromSQL:
		return ClassifyDDL(sql)
	}
	return c.cap
}

var (
	destructiveDDL = regexp.MustCompile(`(?i)\b(DROP|TRUNCATE)\b`)
	sqlComment     = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)
	sqlString      = regexp.MustCompile(`'(?:[^']|'')*'`)
	ddlClassifier  = migration.NewRegexClassifier()
)

// ClassifyDDL classes a schema change (§5.2): any DROP or TRUNCATE is
// destructive (pg_sage cannot tell a unique index from another here);
// anything the migration classifier flags for its lock or rewrite is
// locking; the rest is additive. No SQL is the worst case, destructive.
func ClassifyDDL(sql string) Capability {
	text := sqlString.ReplaceAllString(sqlComment.ReplaceAllString(sql, " "), "''")
	if strings.TrimSpace(text) == "" || destructiveDDL.MatchString(text) {
		return CapDDLDestructive
	}
	for _, c := range ddlClassifier.Classify(text, oldestSupportedPG) {
		if c.RuleID != "ddl_missing_lock_timeout" {
			return CapDDLLocking
		}
	}
	return CapDDLAdditive
}

// oldestSupportedPG classifies for the oldest server pg_sage supports, so
// a rule that only bites on older majors is never missed.
const oldestSupportedPG = 140000
