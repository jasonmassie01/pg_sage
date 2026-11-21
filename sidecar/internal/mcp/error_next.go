package mcp

import (
	"encoding/json"
	"math"
	"time"
)

// What an agent can do about a failed tool call (ux-05f). Every tool error
// keeps code, reason and message; these keys are added where they apply:
//
//   - next: {"tool": name, "arguments": {...}} to call next, the failed
//     tool itself with "required" for arguments to fix, or "wait" when
//     the condition clears with time or a person's action;
//   - retry_after: the known wait in whole seconds (at least 1);
//   - approval_url: the dashboard page where a person decides.
//
// internal_error and unknown codes carry none of them. mcp-01a gives
// every other reason a next rule (errorNextRules) and held results the
// same approval_url plus status and handle (held_result.go).

// unavailableRetry is the wait suggested for a dependency that is down.
const unavailableRetry = 30 * time.Second

// notFoundLists name the tool that lists what a missing object could be.
var notFoundLists = map[string]string{"decide_fact": "list_facts",
	"sre_get_investigation": "sre_list_incidents", "sre_get_evidence": "sre_list_incidents",
	"sre_get_transcript": "sre_list_incidents", "sre_review_investigation": "sre_list_incidents",
	"sre_similar_incidents": "sre_list_incidents", "sre_get_runbook": "sre_list_runbooks",
	"sre_runbook_runs": "sre_list_runbooks", "sre_get_slo": "sre_list_slos"}

// approvalPages are the dashboard pages where a person makes the decision
// an approve-scope tool asks for; other approvals wait in Actions.
var approvalPages = map[string]string{"decide_fact": "#/facts",
	"declare_table_contract": "#/facts", "register_consumer": "#/facts",
	"sre_downgrade_autonomy": "#/advanced/autonomy", "sre_review_investigation": "#/incidents",
	"sre_rollback_statistics": "#/actions"}

const defaultApprovalPage = "#/actions?tab=pending"

// withRetryAfter records the known wait before a retry.
func (e *rpcError) withRetryAfter(wait time.Duration) *rpcError {
	e.retryAfter, e.retryKnown = wait, true
	return e
}

// unconfigured is an unavailable feature a person must switch on first.
func unconfigured(message string) *rpcError {
	failed := failure(codeUnavailable, message)
	failed.unconfigured = true
	return failed
}

// callError is a failed tools/call of tool: toolError plus what to do next.
func (s *Server) callError(tool string, arguments json.RawMessage,
	failed *rpcError) map[string]any {
	out := toolError(failed)
	detail := out["structuredContent"].(map[string]any)["error"].(map[string]any)
	if next := s.nextStep(tool, arguments, failed); next != nil {
		detail["next"] = next
	}
	if seconds, ok := retryAfterSeconds(failed); ok {
		detail["retry_after"] = seconds
	}
	if failed.Code == codeApprovalReserved {
		detail["approval_url"] = approvalPage(tool, arguments)
	}
	return out
}

// nextRule is what an agent calls after a failure of one reason; nil
// when, for this tool and failure, nothing it calls would help.
type nextRule func(s *Server, tool string, arguments json.RawMessage, failed *rpcError) any

// errorNextRules give every reason in errorReasons a next (mcp-01a),
// except noNextCodes. TestEveryErrorReasonHasANextRuleOrIsExempt fails on
// a reason with neither.
var errorNextRules = map[int]nextRule{
	codeInvalidParams: fixArguments, codeRunbookRejected: fixArguments,
	codeApprovalReserved: func(_ *Server, _ string, arguments json.RawMessage,
		_ *rpcError) any {
		return map[string]any{"tool": toolGetInbox, "arguments": inboxArguments(arguments)}
	},
	codeNotPermitted: listDatabasesNext, codeDatabaseRequired: listDatabasesNext,
	codeUnknownDatabase: listDatabasesNext, codeNotFound: findAgain,
	codeUnavailable: func(_ *Server, _ string, _ json.RawMessage, failed *rpcError) any {
		if failed.unconfigured { // a person must switch the feature on first
			return nil
		}
		return "wait"
	},
	codeRateLimited: waitNext, codeTooManyInvestigations: waitNext, codeConflict: waitNext,
	codeScopeRequired: waitNext,
}

// noNextCodes carry no next: an internal error says nothing an agent can
// act on (ux-05f).
var noNextCodes = map[int]bool{codeInternal: true}

// nextStep is the next key for failed, or nil when there is none.
func (s *Server) nextStep(tool string, arguments json.RawMessage, failed *rpcError) any {
	rule, ok := errorNextRules[failed.Code]
	if !ok {
		return nil
	}
	return rule(s, tool, arguments, failed)
}

// waitNext: the condition clears with time or a person's action.
func waitNext(*Server, string, json.RawMessage, *rpcError) any { return "wait" }

// listDatabasesNext: name a database the caller may use.
func listDatabasesNext(*Server, string, json.RawMessage, *rpcError) any {
	return map[string]any{"tool": "list_databases", "arguments": map[string]any{}}
}

// fixArguments: call the same tool again with its required arguments
// (and, for request_change, an example intent).
func fixArguments(s *Server, tool string, arguments json.RawMessage, _ *rpcError) any {
	required, ok := s.requiredArguments(tool)
	if !ok {
		return nil
	}
	next := map[string]any{"tool": tool, "required": required}
	if tool == "request_change" {
		next["example"] = intentExample(arguments)
	}
	return next
}

// findAgain: list what the missing object could be, or else call the
// same tool again with a corrected identifier. A tool that takes no
// required argument has no identifier to correct, so it gets none.
func findAgain(s *Server, tool string, arguments json.RawMessage, failed *rpcError) any {
	if list, ok := notFoundLists[tool]; ok {
		return map[string]any{"tool": list, "arguments": map[string]any{}}
	}
	if required, ok := s.requiredArguments(tool); !ok || len(required) == 0 {
		return nil
	}
	return fixArguments(s, tool, arguments, failed)
}

// retryAfterSeconds is the wait in whole seconds, rounded up to at least
// one: a known reset, or the default wait for a dependency that is down.
func retryAfterSeconds(failed *rpcError) (int64, bool) {
	wait, known := failed.retryAfter, failed.retryKnown
	if !known && failed.Code == codeUnavailable && !failed.unconfigured {
		wait, known = unavailableRetry, true
	}
	if !known {
		return 0, false
	}
	return max(1, int64(math.Ceil(wait.Seconds()))), true
}

// requiredArguments are the required properties of tool's input schema
// (an empty list when it has none); false for a tool the server lacks.
func (s *Server) requiredArguments(tool string) ([]string, bool) {
	for _, defined := range s.tools {
		if defined.Name != tool {
			continue
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if json.Unmarshal(defined.InputSchema, &schema) != nil || schema.Required == nil {
			return []string{}, true
		}
		return schema.Required, true
	}
	return nil, false
}

// inboxArguments carry the database the failed call named, if any.
func inboxArguments(arguments json.RawMessage) map[string]any {
	var call struct {
		Database string `json:"database"`
	}
	if json.Unmarshal(arguments, &call) != nil || call.Database == "" {
		return map[string]any{}
	}
	return map[string]any{"database": call.Database}
}

// approvalPage is where a person decides what tool asked for;
// request_change decides by the kind of its intent.
func approvalPage(tool string, arguments json.RawMessage) string {
	if tool == "request_change" {
		tool = intentKind(arguments)
	}
	if page, ok := approvalPages[tool]; ok {
		return page
	}
	return defaultApprovalPage
}
