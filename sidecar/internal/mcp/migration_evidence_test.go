package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// migration-03 (no database): the evidence id and reason an MCP
// apply_migration hands its store. No concurrent access here: each test
// owns its executor and fake store; the parallel case runs on real
// PostgreSQL in migration_run_ledger_test.go.

func TestBlockedMigrationRecordsTheGateReason(t *testing.T) {
	parsetree.Require(t)
	store := &recordingIntentStore{}
	gate := &recordingProductionGate{decision: policy.Decision{Verdict: policy.VerdictBlocked,
		Reason: policy.ReasonChangeClassNotAllowed, EvidenceID: "ev-blocked"}}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner(), gate)
	_, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
	require.NoError(t, err)
	require.Equal(t, "blocked", store.migration.Verdict)
	require.Equal(t, "ev-blocked", store.migration.EvidenceID)
	require.Equal(t, "change_class_not_allowed", store.migration.Reason)
	require.Equal(t, ledgerEmailSQL, store.migration.SourceSQL)
}

func TestBlockedMigrationWithoutEvidenceGetsAFreshID(t *testing.T) {
	parsetree.Require(t)
	ids := make([]string, 0, 2)
	for range 2 {
		store := &recordingIntentStore{}
		gate := &recordingProductionGate{decision: policy.Decision{
			Verdict: policy.VerdictBlocked, Reason: policy.ReasonChangeClassNotAllowed}}
		executor := NewProductionIntentExecutor(store, plan.NewPlanner(), gate)
		result, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
		require.NoError(t, err)
		outcome := result.(MigrationOutcome)
		require.Equal(t, "blocked", outcome.Verdict)
		require.NotEmpty(t, store.migration.EvidenceID)
		require.Equal(t, store.migration.EvidenceID, outcome.EvidenceID)
		ids = append(ids, outcome.EvidenceID)
	}
	require.NotEqual(t, ids[0], ids[1])
}

func TestRecommendOnlyMigrationNeverUsesASharedEvidenceLiteral(t *testing.T) {
	parsetree.Require(t)
	store := &recordingIntentStore{}
	gate := &recordingProductionGate{decision: policy.Decision{
		Verdict: policy.VerdictQueueApproval}}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner(), gate)
	ids := make([]string, 0, 2)
	for range 2 {
		result, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
		require.NoError(t, err)
		outcome := result.(MigrationOutcome)
		require.Equal(t, "recommend_only", store.migration.Verdict)
		require.True(t, strings.HasPrefix(store.migration.EvidenceID, "ev_"),
			"want a generated ledger evidence id, got %q", store.migration.EvidenceID)
		require.Equal(t, store.migration.EvidenceID, outcome.EvidenceID)
		ids = append(ids, store.migration.EvidenceID)
	}
	require.NotEqual(t, ids[0], ids[1])
}

func TestRecommendOnlyMigrationKeepsTheActionEvidence(t *testing.T) {
	parsetree.Require(t)
	store := &recordingIntentStore{}
	gate := &recordingProductionGate{decision: policy.Decision{
		Verdict: policy.VerdictQueueApproval, EvidenceID: "ev-queue"}}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner(), gate)
	result, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
	require.NoError(t, err)
	require.Equal(t, "ev-queue", store.migration.EvidenceID)
	require.Equal(t, "ev-queue", result.(MigrationOutcome).EvidenceID)
}

// The Execute path (a gate that already authorized the intent) had the
// same gap: a gate without a decision recorder hands back no evidence id,
// and every such plan was written under one empty evidence_id.
func TestAuthorizedMigrationWithoutEvidenceGetsAFreshID(t *testing.T) {
	parsetree.Require(t)
	ids := make([]string, 0, 2)
	for range 2 {
		store := &recordingIntentStore{}
		executor := NewProductionIntentExecutor(store, plan.NewPlanner())
		result, err := executor.Execute(context.Background(), migrationRequest(),
			policy.Decision{Verdict: policy.VerdictExecute})
		require.NoError(t, err)
		outcome := result.(MigrationOutcome)
		require.Equal(t, "recommend_only", store.migration.Verdict)
		require.True(t, strings.HasPrefix(store.migration.EvidenceID, "ev_"),
			"want a generated ledger evidence id, got %q", store.migration.EvidenceID)
		require.Equal(t, store.migration.EvidenceID, outcome.EvidenceID)
		require.Equal(t, ledgerEmailSQL, store.migration.SourceSQL)
		require.Empty(t, store.migration.Reason)
		ids = append(ids, outcome.EvidenceID)
	}
	require.NotEqual(t, ids[0], ids[1])
}

func TestAuthorizedMigrationKeepsTheDecisionEvidence(t *testing.T) {
	parsetree.Require(t)
	store := &recordingIntentStore{}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner())
	result, err := executor.Execute(context.Background(), migrationRequest(),
		policy.Decision{Verdict: policy.VerdictExecute, EvidenceID: "ev-decision"})
	require.NoError(t, err)
	require.Equal(t, "ev-decision", store.migration.EvidenceID)
	require.Equal(t, "ev-decision", result.(MigrationOutcome).EvidenceID)
}

func TestBlockedMigrationStoreErrorIsReturned(t *testing.T) {
	parsetree.Require(t)
	storeErr := errors.New("sage.migration_run unavailable")
	store := &failingMigrationStore{err: storeErr}
	gate := &recordingProductionGate{decision: policy.Decision{
		Verdict: policy.VerdictBlocked, Reason: policy.ReasonChangeClassNotAllowed}}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner(), gate)
	result, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
	require.ErrorIs(t, err, storeErr)
	outcome := result.(MigrationOutcome)
	require.Empty(t, outcome.Verdict, "a failed ledger write reports no verdict")
	require.Empty(t, outcome.EvidenceID)
	require.Equal(t, "blocked", store.migration.Verdict)
}

func TestRecordMigrationWithoutAPoolFailsClosed(t *testing.T) {
	var access *PostgresAccess
	err := access.RecordMigration(context.Background(), MigrationRecord{
		EvidenceID: "ev", Verdict: "blocked",
	})
	require.ErrorIs(t, err, ErrProductionDependencyUnavailable)
	require.ErrorIs(t, NewPostgresAccess(nil).RecordMigration(context.Background(),
		MigrationRecord{}), ErrProductionDependencyUnavailable)
}

type failingMigrationStore struct {
	recordingIntentStore
	err error
}

func (store *failingMigrationStore) RecordMigration(
	_ context.Context, record MigrationRecord,
) error {
	store.migration = record
	return store.err
}
