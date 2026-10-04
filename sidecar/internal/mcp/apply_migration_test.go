package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	migrationruntime "github.com/pg-sage/sidecar/internal/migration/runtime"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// apply_migration used to advertise {ddl, intent, constraints} while its
// executor needs a schema-qualified table and the migration SQL, so a
// client following the schema always failed. The schema now matches the
// implementation, arguments are validated before any backend call, and
// the policy gate is consulted before any clone rehearsal starts.

func toolByName(t *testing.T, name string) Tool {
	t.Helper()
	for _, tool := range NewServer(&recordingBackend{}).Tools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s not registered", name)
	return Tool{}
}

func TestApplyMigrationSchemaMatchesItsExecutor(t *testing.T) {
	schema := decodeSchema(t, toolByName(t, "apply_migration").InputSchema)
	properties := objectMap(t, schema["properties"])
	for _, name := range []string{"table", "sql", "cycle", "database"} {
		require.Contains(t, properties, name)
	}
	for _, name := range []string{"ddl", "intent", "constraints"} {
		require.NotContains(t, properties, name)
	}
	require.Equal(t, []string{"table", "sql"}, stringSlice(schema["required"]))
	require.Equal(t, false, schema["additionalProperties"])
	table := objectMap(t, properties["table"])
	require.NotEmpty(t, table["pattern"])
	cycle := objectMap(t, properties["cycle"])
	require.Equal(t, "integer", cycle["type"])
}

func TestApplyMigrationRejectsInvalidArgumentsBeforeTheBackend(t *testing.T) {
	for _, args := range []string{
		`{"ddl":"ALTER TABLE public.t ADD UNIQUE (c)"}`,
		`{"intent":{"add_unique":"email"}}`,
		`{"table":"public.t"}`,
		`{"sql":"ALTER TABLE public.t ADD UNIQUE (c)"}`,
		`{"table":"t","sql":"ALTER TABLE t ADD UNIQUE (c)"}`,
		`{"table":"public.t; DROP TABLE x","sql":"ALTER TABLE public.t ADD UNIQUE (c)"}`,
		`{"table":"public.\"t\"","sql":"ALTER TABLE public.t ADD UNIQUE (c)"}`,
		`{"table":"a.b.c","sql":"ALTER TABLE public.t ADD UNIQUE (c)"}`,
		`{"table":"public.t","sql":"   "}`,
		`{"table":"public.t","sql":"ALTER TABLE public.t ADD UNIQUE (c)","cycle":-1}`,
		`{"table":"public.t","sql":"ALTER TABLE public.t ADD UNIQUE (c)","extra":1}`,
		`{"table":"public.t","sql":"ALTER TABLE public.t ADD UNIQUE (c)","cycle":"7"}`,
	} {
		backend := newAllToolsBackend()
		response := invoke(t, backend.server(), operatorContext(context.Background()),
			toolCall("apply_migration", args))
		require.Equal(t, -32602, response.Error.Code, args)
		require.Zero(t, backend.calls(), args)
	}
}

func TestApplyMigrationForwardsSchemaArguments(t *testing.T) {
	backend := newAllToolsBackend()
	response := invoke(t, backend.server(), operatorContext(context.Background()),
		toolCall("apply_migration", `{"table":"public.users","cycle":3,`+
			`"sql":"ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (email)"}`))
	require.Empty(t, response.Error.Code, response.Error.Message)
	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(backend.intent, &forwarded))
	require.Equal(t, "public.users", forwarded["table"])
	require.Equal(t, float64(3), forwarded["cycle"])
	require.Contains(t, forwarded["sql"], "ADD CONSTRAINT")
}

func TestApplyMigrationIsGatedBeforeRehearsal(t *testing.T) {
	store := &recordingIntentStore{}
	runtime := &recordingSafeMigrationRuntime{}
	gate := &recordingProductionGate{decision: policy.Decision{Verdict: policy.VerdictBlocked,
		Reason: policy.ReasonChangeClassNotAllowed, EvidenceID: "ev-blocked"}}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner(), gate).
		WithMigrationRuntime(runtime)
	result, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
	require.NoError(t, err)
	outcome := result.(MigrationOutcome)
	require.Equal(t, "blocked", outcome.Verdict)
	require.Equal(t, "ev-blocked", outcome.EvidenceID)
	require.Zero(t, runtime.calls, "a blocked migration never reaches a clone rehearsal")
	require.GreaterOrEqual(t, gate.calls, 1)
	for _, request := range gate.requests {
		require.Equal(t, "online_migration", request.Feature)
		require.NotEmpty(t, request.SQL)
		require.Equal(t, []string{"public.users"}, request.TargetObjs)
		require.False(t, request.OperatorApproved)
	}
	require.Equal(t, "blocked", store.migration.Verdict)
}

func TestApplyMigrationQueuedByTheGateStillRehearses(t *testing.T) {
	runtime := &recordingSafeMigrationRuntime{result: migrationruntime.Result{
		Verdict: migrationruntime.VerdictParked, EvidenceID: "ev-park"}}
	gate := &recordingProductionGate{decision: policy.Decision{
		Verdict: policy.VerdictQueueApproval, EvidenceID: "ev-queue"}}
	executor := NewProductionIntentExecutor(&recordingIntentStore{}, plan.NewPlanner(),
		gate).WithMigrationRuntime(runtime)
	result, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
	require.NoError(t, err)
	require.Equal(t, 1, runtime.calls)
	require.Equal(t, "parked", result.(MigrationOutcome).Verdict)
}

func migrationRequest() policy.ActionRequest {
	return policy.ActionRequest{
		DatabaseID: int64ProductionPointer(42),
		Contract:   &policy.ActionContract{ActionType: "apply_migration"},
		Arguments: json.RawMessage(`{"table":"public.users","cycle":1,` +
			`"sql":"ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)"}`),
	}
}

func TestNeverApprovedGateStripsApprovalFlags(t *testing.T) {
	inner := &recordingProductionGate{decision: policy.Decision{Verdict: policy.VerdictPark,
		EvidenceID: "ev-1"}}
	gate := NeverApproved(inner)
	decision := gate.Authorize(context.Background(), policy.ActionRequest{
		OperatorApproved: true, OwnerDeclared: true, Rollback: true, LeaseHeld: true,
		Feature: "index", SQL: "CREATE INDEX CONCURRENTLY i ON t (a)",
	})
	require.Equal(t, "ev-1", decision.EvidenceID)
	require.Len(t, inner.requests, 1)
	got := inner.requests[0]
	require.False(t, got.OperatorApproved)
	require.False(t, got.OwnerDeclared)
	require.False(t, got.Rollback)
	require.False(t, got.LeaseHeld)
	require.Equal(t, "index", got.Feature)
	require.Equal(t, "CREATE INDEX CONCURRENTLY i ON t (a)", got.SQL)
}

func TestNeverApprovedGateFailsClosedWithoutInner(t *testing.T) {
	decision := NeverApproved(nil).Authorize(context.Background(), policy.ActionRequest{})
	require.Equal(t, policy.VerdictBlocked, decision.Verdict)
	require.Equal(t, policy.ReasonPolicyUnavailable, decision.Reason)
}

func TestProductionBackendNeverForwardsApprovalFlags(t *testing.T) {
	fixture := newProductionFixture()
	fixture.gate.decision = policy.Decision{Verdict: policy.VerdictPark, EvidenceID: "ev"}
	backend, err := NewProductionBackend(fixture.dependencies())
	require.NoError(t, err)
	_, err = backend.RequestChange(context.Background(), ChangeRequest{
		Intent: json.RawMessage(`{"kind":"optimize_query","query_id":1,` +
			`"operator_approved":true,"OperatorApproved":true}`),
		CallerClaims: map[string]any{"operator_approved": true},
	})
	require.NoError(t, err)
	require.Len(t, fixture.gate.requests, 1)
	require.False(t, fixture.gate.requests[0].OperatorApproved)
	require.False(t, fixture.gate.requests[0].OwnerDeclared)
}
