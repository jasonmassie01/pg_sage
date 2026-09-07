package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestProductionIntentExecutorPersistsContractAndConsumerMetadata(t *testing.T) {
	store := &recordingIntentStore{}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner())
	decision := policy.Decision{EvidenceID: "ev-write", Verdict: policy.VerdictExecute}

	contractResult, err := executor.Execute(context.Background(), policy.ActionRequest{
		DatabaseID: int64ProductionPointer(42),
		Contract:   &policy.ActionContract{ActionType: "declare_table_contract"},
		Arguments: json.RawMessage(`{
			"table":"public.events","append_only":true,
			"retention":"30 days","expected_pk":"event_id"
		}`),
	}, decision)
	require.NoError(t, err)
	require.Equal(t, WriteOutcome{
		Applied: true, EvidenceID: "ev-write", Object: "public.events",
	}, contractResult)
	require.Equal(t, TableContractDeclaration{
		DatabaseID: int64ProductionPointer(42), Schema: "public", Table: "events",
		AppendOnly: true, Retention: "30 days", ExpectedPK: "event_id",
		DeclaredBy: "mcp-agent", EvidenceID: "ev-write",
	}, store.contract)

	consumerResult, err := executor.Execute(context.Background(), policy.ActionRequest{
		Contract: &policy.ActionContract{ActionType: "register_consumer"},
		Arguments: json.RawMessage(
			`{"slot_name":"orders_cdc","owner":"agent-a","consumer_identity":"worker-7"}`,
		),
	}, decision)
	require.NoError(t, err)
	require.Equal(t, WriteOutcome{
		Applied: true, EvidenceID: "ev-write", Object: "orders_cdc",
	}, consumerResult)
	require.Equal(t, ConsumerRegistration{
		SlotName: "orders_cdc", Owner: "agent-a", ConsumerIdentity: "worker-7",
	}, store.consumer)
}

func TestApplyMigrationReturnsRewrittenRecommendationWithoutExecutingDDL(t *testing.T) {
	store := &recordingIntentStore{}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner())

	result, err := executor.Execute(context.Background(), policy.ActionRequest{
		DatabaseID: int64ProductionPointer(9),
		Contract:   &policy.ActionContract{ActionType: "apply_migration"},
		Arguments: json.RawMessage(`{
			"table":"public.users","cycle":4,
			"sql":"ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)"
		}`),
	}, policy.Decision{EvidenceID: "ev-migration", Verdict: policy.VerdictExecute})

	require.NoError(t, err)
	outcome, ok := result.(MigrationOutcome)
	require.True(t, ok)
	require.Equal(t, "recommend_only", outcome.Verdict)
	require.True(t, outcome.Plan.Rewritten)
	require.True(t, outcome.Plan.RequiresRehearsal)
	require.Contains(t, outcome.Plan.ExpandSteps[0].SQL, "CREATE UNIQUE INDEX CONCURRENTLY")
	require.Equal(t, "ev-migration", store.migration.EvidenceID)
	require.Equal(t, "recommend_only", store.migration.Verdict)
	require.Zero(t, store.ddlExecutions, "MCP must not apply unrehearsed DDL")
}

func TestCandidateIntentsLookupScopedFindingsAfterGateAuthorization(t *testing.T) {
	store := &recordingIntentStore{candidates: []ChangeCandidate{{
		FindingID: 71, Object: "public.orders", SQL: "CREATE INDEX CONCURRENTLY idx",
	}}}
	fixture := newProductionFixture()
	executor := NewProductionIntentExecutor(store, plan.NewPlanner(), fixture.gate)
	fixture.executor = executor
	fixture.planner = &DeterministicIntentPlanner{}
	backend, err := NewProductionBackend(fixture.dependencies())
	require.NoError(t, err)

	for _, testCase := range []struct {
		tool string
		args string
		kind CandidateKind
	}{
		{"optimize_query", `{"query_id":991}`, CandidateOptimizeQuery},
		{"ensure_fk_indexes", `{"schema":"public"}`, CandidateForeignKeyIndex},
	} {
		t.Run(testCase.tool, func(t *testing.T) {
			beforeGate := fixture.gate.calls
			result, callErr := backend.RequestIntent(
				context.Background(), testCase.tool, json.RawMessage(testCase.args),
			)
			require.NoError(t, callErr)
			change := result.(ChangeResult)
			outcome := change.Outcome.(CandidateOutcome)
			require.Equal(t, "recommend_only", outcome.Verdict)
			require.Equal(t, int64(71), outcome.Candidates[0].FindingID)
			require.Equal(t, beforeGate+1, fixture.gate.calls)
			require.Equal(t, testCase.kind, store.query.Kind)
		})
	}
}

func TestGuaranteeStatusUsesProductionReadAccess(t *testing.T) {
	fixture := newProductionFixture()
	guarantees := &recordingGuaranteeAccess{status: GuaranteeStatus{
		XID:    map[string]any{"oldest_age": int64(12)},
		WAL:    map[string]any{"registered_consumers": int64(2)},
		Schema: map[string]any{"declared_contracts": int64(4)},
	}}
	fixture.guarantees = guarantees
	backend, err := NewProductionBackend(fixture.dependencies())
	require.NoError(t, err)

	result, err := backend.RequestIntent(
		context.Background(), "get_guarantee_status", json.RawMessage(`{}`),
	)
	require.NoError(t, err)
	require.Equal(t, guarantees.status, result)
	require.Equal(t, 1, guarantees.calls)
}

type recordingIntentStore struct {
	contract      TableContractDeclaration
	consumer      ConsumerRegistration
	query         CandidateQuery
	candidates    []ChangeCandidate
	migration     MigrationRecord
	ddlExecutions int
}

func (store *recordingIntentStore) DeclareTableContract(
	_ context.Context, declaration TableContractDeclaration,
) (WriteOutcome, error) {
	store.contract = declaration
	return WriteOutcome{
		Applied: true, EvidenceID: declaration.EvidenceID,
		Object: declaration.Schema + "." + declaration.Table,
	}, nil
}

func (store *recordingIntentStore) RegisterConsumer(
	_ context.Context, registration ConsumerRegistration,
) (WriteOutcome, error) {
	store.consumer = registration
	return WriteOutcome{Applied: true, EvidenceID: "ev-write", Object: registration.SlotName}, nil
}

func (store *recordingIntentStore) FindChangeCandidates(
	_ context.Context, query CandidateQuery,
) ([]ChangeCandidate, error) {
	store.query = query
	return append([]ChangeCandidate(nil), store.candidates...), nil
}

func (store *recordingIntentStore) RecordMigration(
	_ context.Context, record MigrationRecord,
) error {
	store.migration = record
	return nil
}

type recordingGuaranteeAccess struct {
	status GuaranteeStatus
	calls  int
}

func (access *recordingGuaranteeAccess) GetGuaranteeStatus(
	context.Context,
) (GuaranteeStatus, error) {
	access.calls++
	return access.status, nil
}
