package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Every error reason has a next rule (mcp-01a); internal_error alone
// carries none, as ux-05f set.

// No concurrent access tests: the rules are pure result-shaping
// functions; the in-process server tests already cover concurrent calls.
// No state transition tests: an error carries no state.

// reasonCases are a tool and arguments for which each reason has a next.
var reasonCases = map[int]struct{ tool, args string }{
	codeScopeRequired:         {"propose_fact", `{}`},
	codeNotPermitted:          {"get_policy", `{"database":"billing"}`},
	codeNotFound:              {"sre_get_slo", `{"slo_id":"x"}`},
	codeApprovalReserved:      {"decide_fact", `{"database":"orders"}`},
	codeDatabaseRequired:      {"get_policy", `{}`},
	codeUnknownDatabase:       {"get_policy", `{"database":"nope"}`},
	codeConflict:              {"specialist_request_remediation", `{}`},
	codeUnavailable:           {"whatif_index", `{}`},
	codeRunbookRejected:       {"sre_draft_runbook", `{}`},
	codeInvalidParams:         {"get_ledger", `{"filter":"x"}`},
	codeRateLimited:           {"agent_query", `{}`},
	codeTooManyInvestigations: {"specialist_open_investigation", `{}`},
}

func TestEveryErrorReasonHasANextRuleOrIsExempt(t *testing.T) {
	require.Equal(t, map[int]bool{codeInternal: true}, noNextCodes,
		"internal_error is the only reason without a next")
	for code, reason := range errorReasons {
		_, ruled := errorNextRules[code]
		if noNextCodes[code] {
			require.False(t, ruled, "%s is exempt yet has a rule", reason)
			detail := callErrorDetail(t, "get_policy", `{}`, failure(code, reason))
			require.NotContains(t, detail, "next", reason)
			continue
		}
		require.True(t, ruled, "%s has no next rule", reason)
		example, ok := reasonCases[code]
		require.True(t, ok, "%s has no test case", reason)
		detail := callErrorDetail(t, example.tool, example.args, failure(code, reason))
		require.Contains(t, detail, "next", reason)
		require.NotNil(t, detail["next"], reason)
		require.Equal(t, reason, detail["reason"])
	}
	require.Len(t, reasonCases, len(errorReasons)-len(noNextCodes))
}

func TestNotFoundWithoutAListToolNamesTheSameToolToFix(t *testing.T) {
	detail := callErrorDetail(t, "specialist_investigation_status",
		`{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
		failure(codeNotFound, "no such investigation"))
	next := objectMap(t, detail["next"])
	require.Equal(t, "specialist_investigation_status", next["tool"])
	require.Equal(t, []any{"investigation_id"}, next["required"])
	listed := callErrorDetail(t, "sre_get_slo", `{}`, failure(codeNotFound, "no such slo"))
	require.Equal(t, "sre_list_slos", objectMap(t, listed["next"])["tool"],
		"a list tool still wins where one exists")
}

func TestRetryAfterKeepsTheServerValueAtTheBoundaries(t *testing.T) {
	for wait, want := range map[time.Duration]float64{time.Second: 1, 3600 * time.Second: 3600,
		1500 * time.Millisecond: 2, 59 * time.Minute: 3540} {
		detail := callErrorDetail(t, "agent_query", `{}`,
			failure(codeRateLimited, "rate limited").withRetryAfter(wait))
		require.Equal(t, want, detail["retry_after"], wait.String())
		require.Equal(t, "wait", detail["next"], wait.String())
	}
}

func TestRetryAfterRuleForEachReason(t *testing.T) {
	for code, reason := range errorReasons {
		detail := callErrorDetail(t, "get_policy", `{}`, failure(code, reason))
		if code == codeUnavailable {
			require.Equal(t, float64(30), detail["retry_after"], reason)
			continue
		}
		require.NotContains(t, detail, "retry_after", "%s without a known wait", reason)
	}
	off := callErrorDetail(t, "ask_sage", `{}`, unconfigured("ask is disabled"))
	require.NotContains(t, off, "retry_after", "a switched-off feature is not retried")
	require.NotContains(t, off, "next", "a person must switch it on first")
}

func TestInvalidArgumentsOfAToolWithAnExampleCarryIt(t *testing.T) {
	detail := callErrorDetail(t, "request_change", `{"intent":{"kind":"register_consumer"}}`,
		failure(codeInvalidParams, "invalid arguments"))
	next := objectMap(t, detail["next"])
	example := objectMap(t, objectMap(t, next["example"])["intent"])
	require.Equal(t, "register_consumer", example["kind"])
	other := callErrorDetail(t, "get_ledger", `{"filter":"x"}`,
		failure(codeInvalidParams, "invalid arguments"))
	require.NotContains(t, objectMap(t, other["next"]), "example", "only request_change")
}

func TestErrorKeysAreStillCodeReasonMessageFirst(t *testing.T) {
	out := NewServer(&recordingBackend{}).callError("get_policy", json.RawMessage(`{}`),
		failure(codeInternal, "internal error"))
	raw, err := json.Marshal(out["structuredContent"])
	require.NoError(t, err)
	require.Equal(t, `{"error":{"code":-32603,"message":"internal error",`+
		`"reason":"internal_error"}}`, string(raw), "internal_error carries nothing more")
}
