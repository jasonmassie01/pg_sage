package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestIntentValidationRejectsMalformedOrIncompleteRequests(t *testing.T) {
	executor := NewProductionIntentExecutor(&recordingIntentStore{}, plan.NewPlanner())
	execute := func(action string, arguments string) error {
		_, err := executor.Execute(context.Background(), policy.ActionRequest{
			Contract:  &policy.ActionContract{ActionType: action},
			Arguments: json.RawMessage(arguments),
		}, policy.Decision{Verdict: policy.VerdictExecute, EvidenceID: "ev-invalid"})
		return err
	}
	require.Error(t, execute("declare_table_contract", `{"table":"events"}`))
	require.Error(t, execute("declare_table_contract",
		`{"table":"public.events","retention":{"days":7}}`))
	require.Error(t, execute("register_consumer", `{"slot_name":"orders"}`))
	require.Error(t, execute("apply_migration", `{"table":"users","sql":"ALTER"}`))
	require.Error(t, execute("unsupported", `{}`))
	_, err := executor.Execute(context.Background(), policy.ActionRequest{}, policy.Decision{})
	require.Error(t, err)
}

func TestIntentHelpersHandleAlternateAndInvalidInputs(t *testing.T) {
	retention, err := retentionText(json.RawMessage(`{"interval":"7 days"}`))
	require.NoError(t, err)
	require.Equal(t, "7 days", retention)
	retention, err = retentionText(nil)
	require.NoError(t, err)
	require.Empty(t, retention)
	_, _, err = qualifiedName("events")
	require.Error(t, err)

	_, _, err = intentContract("request_change", json.RawMessage(`{}`))
	require.Error(t, err)
	_, _, err = intentContract("unknown", json.RawMessage(`{}`))
	require.Error(t, err)
	_, err = (FailClosedIntentExecutor{}).Execute(
		context.Background(), policy.ActionRequest{}, policy.Decision{},
	)
	require.Error(t, err)
}

func TestLedgerAndPolicyHelpersCoverLimitsAndNestedMerges(t *testing.T) {
	filter, err := parseLedgerFilter(nil)
	require.NoError(t, err)
	require.Equal(t, 100, filter.Limit)
	_, err = parseLedgerFilter(json.RawMessage(`{"limit":0}`))
	require.Error(t, err)
	_, err = parseLedgerFilter(json.RawMessage(`{"limit":1001}`))
	require.Error(t, err)
	_, err = parseLedgerFilter(json.RawMessage(`{broken`))
	require.Error(t, err)

	current := json.RawMessage(`{
		"allowed_change_classes":["index"],"maintenance_windows":["always"],
		"lock_duration_ceiling_ms":1000,
		"blast_radius":{"max_rows_rewritten":10,"max_tables_per_window":1},
		"budgets":{"storage_bytes":100,"spend_daily":10,"llm_tokens_daily":100},
		"rate_limits":{"max_self_initiated_changes_per_window":1},
		"deadline_overrides":{},"refusal_set":[],
		"unknown_classification":"fail_closed","serialize_mode":"park"
	}`)
	merged, err := mergePolicyDelta(current, json.RawMessage(
		`{"blast_radius":{"max_rows_rewritten":20}}`,
	))
	require.NoError(t, err)
	require.Contains(t, string(merged), `"max_rows_rewritten":20`)
	_, err = mergePolicyDelta(json.RawMessage(`[]`), json.RawMessage(`{}`))
	require.Error(t, err)
	_, err = mergePolicyDelta(current, json.RawMessage(`[]`))
	require.Error(t, err)
}
