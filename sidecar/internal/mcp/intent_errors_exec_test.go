package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Executor refusals the caller can fix (mcp-08 review): a declaration
// that names no schema, a retention without its column, a consumer
// without slot or owner are "fix your request" (-32602) with the reason,
// never an opaque internal error.

func adminContext() context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "user:1", Role: "admin"})
}

func executingServer(t *testing.T, store *recordingIntentStore) *Server {
	t.Helper()
	return productionServer(t, func(d *ProductionDependencies) {
		d.Planner = DeterministicIntentPlanner{}
		d.Executor = NewProductionIntentExecutor(store, plan.NewPlanner())
	})
}

func TestIntentFailureExecutorRefusalsAreInvalidArguments(t *testing.T) {
	cases := []struct{ tool, args, want string }{
		{"declare_table_contract", `{"table":"events","append_only":true}`,
			"schema-qualified"},
		{"declare_table_contract", `{"table":"public.events",` +
			`"retention":{"interval":"30 days"}}`, "retention.column is required"},
		{"declare_table_contract", `{"table":"public.events",` +
			`"retention":{"column":"created_at"}}`, "retention.interval is required"},
		{"register_consumer", `{"slot_name":" ","owner":"agent-a"}`,
			"slot_name and owner are required"},
	}
	for _, c := range cases {
		store := &recordingIntentStore{}
		response := invoke(t, executingServer(t, store), adminContext(),
			toolCall(c.tool, c.args))
		require.Equal(t, codeInvalidParams, response.Error.Code, "%s %s: %s", c.tool,
			c.args, response.Error.Message)
		require.Contains(t, response.Error.Message, c.want, c.args)
		require.Equal(t, TableContractDeclaration{}, store.contract, c.args)
		require.Equal(t, ConsumerRegistration{}, store.consumer, c.args)
	}
}

// Arguments the strict validators cannot see (request_change carries a
// free-form intent) are typed invalid by the executor itself.
func TestIntentFailureExecutorDecodeRefusalsAreTyped(t *testing.T) {
	for _, arguments := range []string{`{"table":5}`,
		`{"table":"public.events","retention":"30 days"}`,
		`{"table":"public.events","retention":{"interval":7,"column":"c"}}`} {
		store := &recordingIntentStore{}
		_, err := declareContract(t, store, arguments)
		require.ErrorIs(t, err, ErrIntentInvalid, arguments)
		require.Equal(t, TableContractDeclaration{}, store.contract, arguments)
	}
	executor := NewProductionIntentExecutor(&recordingIntentStore{}, plan.NewPlanner())
	_, err := executor.Execute(context.Background(), policy.ActionRequest{
		Contract:  &policy.ActionContract{ActionType: "register_consumer"},
		Arguments: json.RawMessage(`{"slot_name":["s"],"owner":"a"}`),
	}, policy.Decision{Verdict: policy.VerdictExecute})
	require.ErrorIs(t, err, ErrIntentInvalid)
}

func TestIntentFailureExecutorAcceptsAValidDeclaration(t *testing.T) {
	store := &recordingIntentStore{}
	response := invoke(t, executingServer(t, store), adminContext(),
		toolCall("declare_table_contract", `{"table":"public.events",`+
			`"retention":{"interval":"30 days","column":"created_at"}}`))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.Equal(t, "created_at", store.contract.RetentionColumn)
	require.Equal(t, "events", store.contract.Table)
}

// register_consumer records consumer_identity (the executor and the store
// take it), so its schema lists it and strict validation accepts it.
func TestStrictRegisterConsumerAcceptsConsumerIdentity(t *testing.T) {
	store := &recordingIntentStore{}
	response := invoke(t, executingServer(t, store), adminContext(),
		toolCall("register_consumer",
			`{"slot_name":"orders_cdc","owner":"agent-a","consumer_identity":"worker-7"}`))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.Equal(t, ConsumerRegistration{
		SlotName: "orders_cdc", Owner: "agent-a", ConsumerIdentity: "worker-7",
	}, store.consumer)

	store = &recordingIntentStore{}
	response = invoke(t, executingServer(t, store), adminContext(),
		toolCall("register_consumer",
			`{"slot_name":"orders_cdc","owner":"agent-a","consumer_identity":7}`))
	require.Equal(t, codeInvalidParams, response.Error.Code, response.Error.Message)
	require.Equal(t, ConsumerRegistration{}, store.consumer)

	for _, tool := range NewServer(&recordingBackend{}).Tools() {
		if tool.Name == "register_consumer" {
			properties := objectMap(t, decodeSchema(t, tool.InputSchema)["properties"])
			require.Contains(t, properties, "consumer_identity")
		}
	}
}

// Integration: the catalog-backed retention checks are typed invalid; a
// database failure while checking is not.
func TestIntentFailureRetentionColumnRefusalsAreTyped(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	table := retentionContractTable(t, pool, "CREATE TABLE {t} (id bigint, note text, "+
		"created_at timestamptz)")
	for _, column := range []string{"note", "absent"} {
		_, err := access.DeclareTableContract(context.Background(),
			retentionDeclaration(table, column))
		require.ErrorIs(t, err, ErrIntentInvalid, column)
	}
	view := table + "_view"
	_, err := pool.Exec(context.Background(),
		"CREATE VIEW "+view+" AS SELECT created_at FROM "+table)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP VIEW IF EXISTS "+view)
	})
	_, err = access.DeclareTableContract(context.Background(),
		retentionDeclaration(view, "created_at"))
	require.ErrorIs(t, err, ErrIntentInvalid)
	require.Contains(t, err.Error(), "is not a table")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = access.DeclareTableContract(cancelled, retentionDeclaration(table, "created_at"))
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrIntentInvalid),
		"a database failure is not the caller's: %v", err)
}
