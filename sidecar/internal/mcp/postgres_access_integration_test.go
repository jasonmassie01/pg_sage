package mcp

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

var (
	mcpSchemaOnce sync.Once
	mcpSchemaErr  error
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/mcp"))
}

func TestPostgresAccessPersistsAgentDeclaredMetadata(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()

	contract, err := access.DeclareTableContract(ctx, TableContractDeclaration{
		DatabaseID: int64ProductionPointer(42), Schema: "public", Table: "events",
		AppendOnly: true, Retention: "14 days", ExpectedPK: "event_id",
		Exemptions: json.RawMessage(`["backfill"]`), DeclaredBy: "mcp-agent",
		EvidenceID: "ev-contract-db",
	})
	require.NoError(t, err)
	require.True(t, contract.Applied)
	var appendOnly bool
	var retention string
	var expectedPK string
	require.NoError(t, pool.QueryRow(ctx, `SELECT append_only,
		retention_interval::text, expected_pk FROM sage.table_contract
		WHERE evidence_id=$1`, "ev-contract-db").Scan(
		&appendOnly, &retention, &expectedPK,
	))
	require.True(t, appendOnly)
	require.Equal(t, "14 days", retention)
	require.Equal(t, "event_id", expectedPK)

	consumer, err := access.RegisterConsumer(ctx, ConsumerRegistration{
		SlotName: "orders_cdc", Owner: "agent-a", ConsumerIdentity: "worker-7",
	})
	require.NoError(t, err)
	require.True(t, consumer.Applied)
	var owner, identity string
	require.NoError(t, pool.QueryRow(ctx, `SELECT owner_tag, consumer_identity
		FROM sage.slot_consumer_registry WHERE slot_name=$1`, "orders_cdc").Scan(
		&owner, &identity,
	))
	require.Equal(t, "agent-a", owner)
	require.Equal(t, "worker-7", identity)
}

func TestPostgresAccessFindsScopedOptimizationCandidates(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_identifier, title, detail, recommended_sql)
		VALUES
		('query_optimization','warning','public.orders','slow query',
		 '{"query_id":"991"}', 'CREATE INDEX CONCURRENTLY idx_orders ON public.orders(id)'),
		('missing_fk_index','warning','public.order_items','missing FK index',
		 '{}', 'CREATE INDEX CONCURRENTLY idx_items ON public.order_items(order_id)')`)
	require.NoError(t, err)

	queryCandidates, err := access.FindChangeCandidates(ctx, CandidateQuery{
		Kind: CandidateOptimizeQuery, QueryID: 991,
	})
	require.NoError(t, err)
	require.Len(t, queryCandidates, 1)
	require.Equal(t, "public.orders", queryCandidates[0].Object)
	require.Contains(t, queryCandidates[0].SQL, "CONCURRENTLY")

	fkCandidates, err := access.FindChangeCandidates(ctx, CandidateQuery{
		Kind: CandidateForeignKeyIndex, Schema: "public",
	})
	require.NoError(t, err)
	require.Len(t, fkCandidates, 1)
	require.Equal(t, "public.order_items", fkCandidates[0].Object)
}

func TestPostgresAccessGuaranteeStatusReturnsConcreteCounts(t *testing.T) {
	access, _ := newPostgresIntentAccess(t)
	status, err := access.GetGuaranteeStatus(context.Background())
	require.NoError(t, err)
	require.Contains(t, status.XID, "oldest_database_age")
	require.Contains(t, status.WAL, "registered_consumers")
	require.Contains(t, status.Schema, "declared_contracts")
}

func TestPostgresAccessServesPolicyLedgerValueAndMigrationReads(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()
	_, err := access.policies.Bootstrap(
		ctx, policy.Scope{}, "unattended", "mcp-integration-test",
	)
	require.NoError(t, err)

	current, err := access.GetPolicy(ctx, PolicyRequest{})
	require.NoError(t, err)
	require.Equal(t, int64(1), current.Version)
	require.Equal(t, "unattended", current.Profile)
	proposal, err := access.ProposePolicyChangeDryRun(ctx, PolicyProposalRequest{
		Delta: json.RawMessage(`{"lock_duration_ceiling_ms":1500}`),
	})
	require.NoError(t, err)
	require.Positive(t, proposal.ProposalID)

	_, err = pool.Exec(ctx, `INSERT INTO sage.decision
		(database_id, feature, intent, verdict, risk_tier, reason, evidence_id)
		VALUES (42,'index','optimize_query','parked','safe','test','ev-ledger-db')`)
	require.NoError(t, err)
	ledger, err := access.GetLedger(ctx, LedgerRequest{Filter: json.RawMessage(
		`{"database_id":42,"decision":"parked","feature":"index","limit":5}`,
	)})
	require.NoError(t, err)
	require.Len(t, ledger.Entries, 1)
	require.Equal(t, "ev-ledger-db", ledger.Entries[0].EvidenceID)

	valueResult, err := access.GetValue(ctx)
	require.NoError(t, err)
	require.Contains(t, valueResult, "potential_hours_pending")
	require.NoError(t, access.RecordMigration(ctx, MigrationRecord{
		DatabaseID: int64ProductionPointer(42), EvidenceID: "ev-migration-db",
		SourceSQL: "ALTER TABLE public.users ALTER COLUMN email SET NOT NULL",
		Verdict:   "recommend_only",
	}))
	var verdict string
	require.NoError(t, pool.QueryRow(ctx, `SELECT verdict FROM sage.migration_run
		WHERE evidence_id='ev-migration-db'`).Scan(&verdict))
	require.Equal(t, "recommend_only", verdict)
}

func TestRealPolicyGatePersistsDeclarationsAndAuthorizesCandidateSQL(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()
	validatedSQL := make([]string, 0)
	gate := policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{ExecutorEnabled: true,
				TrustLevel: policy.TrustAutonomous, ExecutionMode: policy.ExecutionAuto}, nil
		},
		ValidateSQL: func(sql string) error {
			validatedSQL = append(validatedSQL, sql)
			return executor.ValidateExecutorSQL(sql)
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return policy.UnattendedProfile(), nil
		},
		RecordDecision: func(
			_ context.Context, request policy.ActionRequest, _ policy.Decision,
		) (string, error) {
			if request.InternalControl {
				return "ev-real-control", nil
			}
			return "ev-real-candidate", nil
		},
	})
	intentExecutor := NewProductionIntentExecutor(access, plan.NewPlanner(), gate)
	backend, err := NewProductionBackend(ProductionDependencies{
		Gate: gate, Planner: DeterministicIntentPlanner{}, Executor: intentExecutor,
		Policy: access, Ledger: access, Value: access, Guarantees: access,
	})
	require.NoError(t, err)

	result, err := backend.RequestIntent(ctx, "declare_table_contract", json.RawMessage(
		`{"table":"public.real_gate_events","append_only":true}`,
	))
	require.NoError(t, err)
	require.Equal(t, "granted", result.(ChangeResult).Decision)
	var persisted bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT append_only FROM sage.table_contract
		WHERE schema_name='public' AND table_name='real_gate_events'`).Scan(&persisted))
	require.True(t, persisted)
	require.Empty(t, validatedSQL, "trusted internal controls must not masquerade as SQL")

	_, err = pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_identifier, title, detail, recommended_sql)
		VALUES ('query_optimization','warning','public.real_gate_orders','slow query',
		'{"query_id":"1776"}',
		'CREATE INDEX CONCURRENTLY idx_real_gate ON public.real_gate_orders(id)')`)
	require.NoError(t, err)
	result, err = backend.RequestIntent(
		ctx, "optimize_query", json.RawMessage(`{"query_id":1776}`),
	)
	require.NoError(t, err)
	outcome := result.(ChangeResult).Outcome.(CandidateOutcome)
	require.Len(t, outcome.Candidates, 1)
	require.Equal(t, "granted", outcome.Candidates[0].Decision)
	require.Equal(t, "ev-real-candidate", outcome.Candidates[0].EvidenceID)
	require.Equal(t, []string{outcome.Candidates[0].SQL}, validatedSQL)
}

func newPostgresIntentAccess(t *testing.T) (*PostgresAccess, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	require.NoError(t, err)
	mcpSchemaOnce.Do(func() {
		mcpSchemaErr = schema.Bootstrap(context.Background(), pool)
	})
	require.NoError(t, mcpSchemaErr)
	t.Cleanup(pool.Close)
	return NewPostgresAccess(pool), pool
}
