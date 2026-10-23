package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// approveScopeTools are a person's decisions: confirming or rejecting facts,
// the declarations pg_sage imports as confirmed facts (table contracts,
// slot consumers), reviews and autonomy downgrades. Agents never hold it.
var approveScopeTools = map[string]bool{"decide_fact": true, "declare_table_contract": true,
	"register_consumer": true, "sre_downgrade_autonomy": true,
	"sre_review_investigation": true}

// proposeScopeTools ask pg_sage to do or record something; pg_sage still
// decides through policy.Gate or a person's approval.
var proposeScopeTools = map[string]bool{"propose_policy_change": true, "request_change": true,
	"optimize_query": true, "apply_migration": true, "ensure_fk_indexes": true,
	"set_maintenance_policy": true, "sre_propose_action": true,
	"sre_request_execution": true, "sre_draft_runbook": true, "sre_compile_runbook": true,
	"sre_evaluate_autonomy": true, "propose_fact": true, "mark_object": true,
	"report_source_fix": true, "specialist_request_remediation": true,
	"agent_request_capability": true}

var (
	knownToolsOnce sync.Once
	knownToolNames map[string]bool
)

func knownTool(name string) bool {
	knownToolsOnce.Do(func() {
		knownToolNames = map[string]bool{}
		for _, tool := range toolDefinitions() {
			knownToolNames[tool.Name] = true
		}
	})
	return knownToolNames[name]
}

// RequiredScope is the scope a tools/call needs; false for an unknown
// tool. request_change takes the scope of the intent it carries.
func RequiredScope(tool string, arguments json.RawMessage) (Scope, bool) {
	if !knownTool(tool) {
		return "", false
	}
	switch {
	case approveScopeTools[tool], tool == "request_change" && approveScopeTools[intentKind(arguments)]:
		return ScopeApprove, true
	case proposeScopeTools[tool]:
		return ScopePropose, true
	}
	return ScopeRead, true
}

func intentKind(arguments json.RawMessage) string {
	var call struct {
		Intent struct {
			Kind string `json:"kind"`
		} `json:"intent"`
	}
	if json.Unmarshal(arguments, &call) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(call.Intent.Kind))
}

// mayCall reports whether the caller holds scope; an unbound caller (a
// library caller with no transport) may only read.
func mayCall(ctx context.Context, scope Scope) bool {
	p, bound := PrincipalFromContext(ctx)
	if !bound {
		return scope == ScopeRead
	}
	return p.Has(scope)
}

// authorizeTool refuses a call the principal's scopes do not cover, with
// a distinguishable code: an agent asking for a person's decision is
// told so (-32005); anyone else lacking a scope gets -32001.
func authorizeTool(ctx context.Context, tool string, arguments json.RawMessage) *rpcError {
	scope, _ := RequiredScope(tool, arguments)
	if mayCall(ctx, scope) {
		return nil
	}
	p, bound := PrincipalFromContext(ctx)
	if bound && scope == ScopeApprove && p.Kind == KindAgent {
		return failure(codeApprovalReserved, "approve scope required: agent principals "+
			"can never approve; a person confirms facts, declarations, reviews and "+
			"autonomy changes in pg_sage")
	}
	return failure(codeScopeRequired, fmt.Sprintf("%s scope required: an operator or "+
		"admin role, or a token with the %s scope", scope, scope))
}
