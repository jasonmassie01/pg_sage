package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/specialist"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Tool errors say what an agent can do next (ux-05f): next names a tool
// (with arguments) or "wait", retry_after is the wait in whole seconds
// where it is known, and approval_url is where a person decides. The keys
// are additive: code, reason and message keep their values.

// No concurrent access tests: the error mapping is a pure function of the
// failure, the tool and its arguments.
// No state transition tests: an error carries no state.

func errorDetail(t *testing.T, response protocolResponse) map[string]any {
	t.Helper()
	return objectMap(t, structuredContent(t, response)["error"])
}

// callErrorDetail maps one failure the way tools/call does.
func callErrorDetail(t *testing.T, tool, arguments string, failed *rpcError) map[string]any {
	t.Helper()
	out := NewServer(&recordingBackend{}).callError(tool, json.RawMessage(arguments), failed)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	var decoded struct {
		StructuredContent struct {
			Error map[string]any `json:"error"`
		} `json:"structuredContent"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	return decoded.StructuredContent.Error
}

func TestToolErrorRateLimitedCarriesTheLimiterReset(t *testing.T) {
	backend := &specialistRecordingBackend{
		err: &specialist.RateLimitError{RetryAfter: 42 * time.Second}}
	response := invoke(t, NewServer(backend), agentCtx(ScopeRead),
		toolCall("specialist_investigation_status",
			specialistToolArgs["specialist_investigation_status"]))
	detail := errorDetail(t, response)
	require.Equal(t, json.Number("-32011"), detail["code"])
	require.Equal(t, "rate_limited", detail["reason"])
	require.Equal(t, json.Number("42"), detail["retry_after"], "the limiter's reset")
	require.Equal(t, "wait", detail["next"])
}

func TestToolErrorRateLimitedGrantParkCarriesItsRetry(t *testing.T) {
	b := &grantToolBackend{result: CapabilityResult{Verdict: VerdictPark,
		ReasonCode: "agent_rate", RetryAfterSeconds: 30}}
	response := invoke(t, NewServer(b), grantAgentCtx(), toolCall("agent_request_capability",
		capabilityArgs))
	detail := errorDetail(t, response)
	require.Equal(t, "rate_limited", detail["reason"])
	require.Equal(t, json.Number("30"), detail["retry_after"])
	require.Equal(t, "wait", detail["next"])
	require.Equal(t, "agent_rate: retry after 30 s", detail["message"])
}

func TestToolErrorRateLimitedAgentQueryKeepsItsRetryAndWaits(t *testing.T) {
	backend := &brokerBackend{result: readapi.Result{Verdict: readapi.VerdictBlocked,
		ReasonCode: "agent_rate", RetryAfterSeconds: 90}}
	response := invoke(t, NewServer(backend), brokerAgentCtx(),
		toolCall("agent_query", `{"database":"app","sql":"SELECT 1"}`))
	detail := errorDetail(t, response)
	require.Equal(t, json.Number("90"), detail["retry_after"])
	require.Equal(t, "wait", detail["next"])
}

func TestToolErrorReservedApprovalPointsAtTheInbox(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}), agentContext(ScopeRead,
		ScopePropose), toolCall("decide_fact", `{"fact_id":12,"decision":"confirm"}`))
	detail := errorDetail(t, response)
	require.Equal(t, "approval_reserved_for_humans", detail["reason"])
	next := objectMap(t, detail["next"])
	require.Equal(t, "get_inbox", next["tool"])
	require.Equal(t, map[string]any{}, objectMap(t, next["arguments"]))
	require.Equal(t, "#/facts", detail["approval_url"])
	_, hasRetry := detail["retry_after"]
	require.False(t, hasRetry, "a person's decision has no retry time")
}

func TestToolErrorReservedApprovalKeepsTheNamedDatabase(t *testing.T) {
	detail := callErrorDetail(t, "sre_downgrade_autonomy",
		`{"database":"orders","reason":"x"}`, failure(codeApprovalReserved, "approve"))
	next := detail["next"].(map[string]any)
	require.Equal(t, "get_inbox", next["tool"])
	require.Equal(t, map[string]any{"database": "orders"}, next["arguments"])
	require.Equal(t, "#/advanced/autonomy", detail["approval_url"])
}

func TestToolErrorReservedApprovalOfAnIntentUsesItsKind(t *testing.T) {
	detail := callErrorDetail(t, "request_change",
		`{"intent":{"kind":"register_consumer","slot_name":"s"}}`,
		failure(codeApprovalReserved, "approve"))
	require.Equal(t, "#/facts", detail["approval_url"])
	unknown := callErrorDetail(t, "request_change", `{"intent":"not an object"}`,
		failure(codeApprovalReserved, "approve"))
	require.Equal(t, "#/actions?tab=pending", unknown["approval_url"])
}

func TestToolErrorInvalidArgumentsNamesTheSameToolAndItsRequiredFields(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}),
		operatorContext(context.Background()),
		toolCall("request_change", `{"intnt":{"kind":"optimize_query"}}`))
	detail := errorDetail(t, response)
	require.Equal(t, "invalid_arguments", detail["reason"])
	next := objectMap(t, detail["next"])
	require.Equal(t, "request_change", next["tool"])
	require.Equal(t, []any{"intent"}, next["required"])
}

func TestToolErrorInvalidArgumentsWithoutRequiredFieldsListsNone(t *testing.T) {
	detail := callErrorDetail(t, "get_policy", `{"x":1}`,
		failure(codeInvalidParams, "invalid arguments: get_policy takes database"))
	next := detail["next"].(map[string]any)
	require.Equal(t, "get_policy", next["tool"])
	require.Equal(t, []any{}, next["required"], "an empty list, not null")
}

func TestToolErrorUnknownCodeCarriesNoNextOrRetry(t *testing.T) {
	for name, failed := range map[string]*rpcError{
		"unknown code":   failure(-31999, "odd"),
		"internal error": failure(codeInternal, "internal error"),
	} {
		detail := callErrorDetail(t, "get_policy", `{}`, failed)
		for _, key := range []string{"next", "retry_after", "approval_url"} {
			_, present := detail[key]
			require.False(t, present, "%s: %s must be absent, not null", name, key)
		}
		require.Equal(t, failed.Message, detail["message"], name)
	}
	require.Equal(t, "error", callErrorDetail(t, "get_policy", `{}`,
		failure(-31999, "odd"))["reason"])
}

func TestToolErrorInvalidArgumentsForAnUnlistedToolHasNoNext(t *testing.T) {
	detail := callErrorDetail(t, "", `{}`, failure(codeInvalidParams, "bad"))
	_, present := detail["next"]
	require.False(t, present, "no tool to name")
}

func TestToolErrorUnavailableWaitsThirtySeconds(t *testing.T) {
	response := invoke(t, productionServer(t, policyFailing(ErrIntentUnavailable)),
		operatorContext(context.Background()), toolCall("get_policy", `{}`))
	detail := errorDetail(t, response)
	require.Equal(t, "unavailable", detail["reason"])
	require.Equal(t, json.Number("30"), detail["retry_after"])
	require.Equal(t, "wait", detail["next"])
}

func TestToolErrorUnconfiguredFeatureIsNotRetried(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}), brokerAgentCtx(),
		toolCall("agent_query", `{"database":"app","sql":"SELECT 1"}`))
	detail := errorDetail(t, response)
	require.Equal(t, "unavailable", detail["reason"])
	require.Equal(t, "agent governance is not configured", detail["message"])
	for _, key := range []string{"next", "retry_after"} {
		_, present := detail[key]
		require.False(t, present, "a person must configure it first: %s", key)
	}
}

func TestToolErrorDatabaseCodesPointAtListDatabases(t *testing.T) {
	for _, code := range []int{codeNotPermitted, codeDatabaseRequired, codeUnknownDatabase} {
		detail := callErrorDetail(t, "get_policy", `{"database":"x"}`, failure(code, "db"))
		next := detail["next"].(map[string]any)
		require.Equal(t, "list_databases", next["tool"], errorReasons[code])
		require.Equal(t, map[string]any{}, next["arguments"], errorReasons[code])
	}
}

func TestToolErrorNotFoundPointsAtTheListTool(t *testing.T) {
	cases := map[string]string{"decide_fact": "list_facts",
		"sre_get_investigation": "sre_list_incidents", "sre_get_runbook": "sre_list_runbooks",
		"sre_get_slo": "sre_list_slos"}
	for tool, list := range cases {
		detail := callErrorDetail(t, tool, `{}`, failure(codeNotFound, "not found"))
		require.Equal(t, list, detail["next"].(map[string]any)["tool"], tool)
	}
	detail := callErrorDetail(t, "get_policy", `{}`, failure(codeNotFound, "not found"))
	_, present := detail["next"]
	require.False(t, present, "no list tool to name")
}

func TestToolErrorWaitCodesHaveNoInventedRetry(t *testing.T) {
	for _, code := range []int{codeConflict, codeTooManyInvestigations, codeScopeRequired,
		codeRateLimited} {
		detail := callErrorDetail(t, "get_policy", `{}`, failure(code, "wait"))
		require.Equal(t, "wait", detail["next"], errorReasons[code])
		_, present := detail["retry_after"]
		require.False(t, present, "%s: no known reset", errorReasons[code])
	}
}

func TestToolErrorRetryAfterRoundsUpToWholeSeconds(t *testing.T) {
	cases := map[time.Duration]float64{0: 1, -time.Second: 1, 300 * time.Millisecond: 1,
		time.Second: 1, time.Second + time.Millisecond: 2, 59500 * time.Millisecond: 60}
	for wait, want := range cases {
		detail := callErrorDetail(t, "get_policy", `{}`,
			failure(codeRateLimited, "rate limited").withRetryAfter(wait))
		require.Equal(t, want, detail["retry_after"], wait.String())
	}
}

// The existing keys are byte-for-byte what they were before next and
// retry_after were added.
func TestToolErrorKeepsCodeReasonAndMessage(t *testing.T) {
	cases := []struct {
		failed *rpcError
		reason string
	}{
		{failure(codeRateLimited, "agent_rate: retry after 30 s"), "rate_limited"},
		{failure(codeApprovalReserved, "approve scope required"), "approval_reserved_for_humans"},
		{failure(codeInvalidParams, "invalid arguments: get_policy takes database"),
			"invalid_arguments"},
		{failure(codeUnavailable, "dependency unavailable"), "unavailable"},
		{failure(codeInternal, "internal error"), "internal_error"},
		{failure(-31999, "odd"), "error"},
	}
	for _, c := range cases {
		out := NewServer(&recordingBackend{}).callError("get_policy", nil, c.failed)
		require.Equal(t, true, out["isError"])
		content := out["content"].([]map[string]any)
		require.Equal(t, []map[string]any{{"type": "text", "text": c.failed.Message}}, content)
		detail := out["structuredContent"].(map[string]any)["error"].(map[string]any)
		require.Equal(t, c.failed.Code, detail["code"], c.reason)
		require.Equal(t, c.reason, detail["reason"])
		require.Equal(t, c.failed.Message, detail["message"], c.reason)
		require.Equal(t, toolError(c.failed)["content"], out["content"], c.reason)
	}
}
