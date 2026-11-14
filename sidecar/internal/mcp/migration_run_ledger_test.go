package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// migration-03: sage.migration_run must accept the "blocked" verdict an
// MCP apply_migration records when the gate refuses it (the CHECK used to
// reject it with 23514), keep the gate's reason, and give every run
// without action evidence its own evidence id instead of one shared
// literal whose conflict update kept the first run's source_sql_hash.

const (
	ledgerEmailSQL = "ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)"
	ledgerNameSQL  = "ALTER TABLE public.users ADD CONSTRAINT users_name_key UNIQUE (name)"
)

func TestRecordMigrationPersistsBlockedVerdictWithReason(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()
	evidenceID := ledger.NewEvidenceID()
	err := access.RecordMigration(ctx, MigrationRecord{
		DatabaseID: int64ProductionPointer(42), EvidenceID: evidenceID,
		SourceSQL: ledgerEmailSQL, Verdict: "blocked",
		Reason: string(policy.ReasonChangeClassNotAllowed),
	})
	require.NoError(t, err)
	row := readMigrationRun(t, pool, evidenceID)
	require.Equal(t, "blocked", row.verdict)
	require.Equal(t, "change_class_not_allowed", row.reason)
	require.Equal(t, sqlHash(ledgerEmailSQL), row.hash)
}

func TestRecordMigrationStillRefusesAnUnknownVerdict(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	evidenceID := ledger.NewEvidenceID()
	err := access.RecordMigration(context.Background(), MigrationRecord{
		EvidenceID: evidenceID, SourceSQL: ledgerEmailSQL, Verdict: "bogus",
	})
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a PostgreSQL error, got %v", err)
	require.Equal(t, "23514", pgErr.Code)
	require.Contains(t, err.Error(), "record MCP migration plan")
	require.Equal(t, 0, countMigrationRuns(t, pool, "evidence_id=$1", evidenceID))
}

func TestRecordMigrationSameEvidenceKeepsTheLatestHashAndReason(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()
	evidenceID := ledger.NewEvidenceID()
	require.NoError(t, access.RecordMigration(ctx, MigrationRecord{
		EvidenceID: evidenceID, SourceSQL: ledgerEmailSQL, Verdict: "blocked",
		Reason: "first",
	}))
	require.NoError(t, access.RecordMigration(ctx, MigrationRecord{
		EvidenceID: evidenceID, SourceSQL: ledgerNameSQL, Verdict: "recommend_only",
		Reason: "second",
	}))
	require.Equal(t, 1, countMigrationRuns(t, pool, "evidence_id=$1", evidenceID))
	row := readMigrationRun(t, pool, evidenceID)
	require.Equal(t, sqlHash(ledgerNameSQL), row.hash)
	require.Equal(t, "recommend_only", row.verdict)
	require.Equal(t, "second", row.reason)
}

func TestRecordMigrationParallelBlockedRunsBothPersist(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	databaseID := uniqueDatabaseID()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			errs[slot] = access.RecordMigration(context.Background(), MigrationRecord{
				DatabaseID: &databaseID, EvidenceID: ledger.NewEvidenceID(),
				SourceSQL: fmt.Sprintf("%s -- %d", ledgerEmailSQL, slot),
				Verdict:   "blocked", Reason: "change_class_not_allowed",
			})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 2, countMigrationRuns(t, pool,
		"database_id=$1 AND verdict='blocked'", databaseID))
}

func TestApplyMigrationIsGatedBeforeRehearsalAgainstPostgres(t *testing.T) {
	parsetree.Require(t)
	access, pool := newPostgresIntentAccess(t)
	evidenceID := ledger.NewEvidenceID()
	runtime := &recordingSafeMigrationRuntime{}
	gate := &recordingProductionGate{decision: policy.Decision{Verdict: policy.VerdictBlocked,
		Reason: policy.ReasonChangeClassNotAllowed, EvidenceID: evidenceID}}
	executor := NewProductionIntentExecutor(access, plan.NewPlanner(), gate).
		WithMigrationRuntime(runtime)
	result, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
	require.NoError(t, err)
	outcome := result.(MigrationOutcome)
	require.Equal(t, "blocked", outcome.Verdict)
	require.Equal(t, evidenceID, outcome.EvidenceID)
	require.Zero(t, runtime.calls, "a blocked migration never reaches a clone rehearsal")
	row := readMigrationRun(t, pool, evidenceID)
	require.Equal(t, "blocked", row.verdict)
	require.Equal(t, "change_class_not_allowed", row.reason)
	require.Equal(t, sqlHash(ledgerEmailSQL), row.hash)
}

func TestRecommendOnlyRunsWithoutEvidenceGetDistinctRows(t *testing.T) {
	parsetree.Require(t)
	access, pool := newPostgresIntentAccess(t)
	databaseID := uniqueDatabaseID()
	gate := &recordingProductionGate{decision: policy.Decision{
		Verdict: policy.VerdictQueueApproval}}
	executor := NewProductionIntentExecutor(access, plan.NewPlanner(), gate)
	ids := make([]string, 0, 2)
	for _, sql := range []string{ledgerEmailSQL, ledgerNameSQL} {
		request := migrationRequestFor(sql)
		request.DatabaseID = &databaseID
		result, err := executor.ExecuteConcrete(context.Background(), request)
		require.NoError(t, err)
		outcome := result.(MigrationOutcome)
		require.Equal(t, "recommend_only", outcome.Verdict)
		require.NotEmpty(t, outcome.EvidenceID)
		require.Equal(t, sqlHash(sql), readMigrationRun(t, pool, outcome.EvidenceID).hash)
		ids = append(ids, outcome.EvidenceID)
	}
	require.NotEqual(t, ids[0], ids[1])
	require.Equal(t, 2, countMigrationRuns(t, pool,
		"database_id=$1 AND verdict='recommend_only'", databaseID))
	require.Equal(t, 0, countMigrationRuns(t, pool,
		"evidence_id='migration-recommendation' AND database_id=$1", databaseID))
}

type migrationRunRow struct {
	verdict string
	reason  string
	hash    string
}

func readMigrationRun(t *testing.T, pool *pgxpool.Pool, evidenceID string) migrationRunRow {
	t.Helper()
	var row migrationRunRow
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT verdict, reason,
		source_sql_hash FROM sage.migration_run WHERE evidence_id=$1`, evidenceID).Scan(
		&row.verdict, &row.reason, &row.hash))
	return row
}

func countMigrationRuns(
	t *testing.T, pool *pgxpool.Pool, predicate string, argument any,
) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.migration_run WHERE `+predicate, argument).Scan(&count))
	return count
}

func sqlHash(sql string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(sql)))
}

func uniqueDatabaseID() int64 {
	return time.Now().UnixNano()
}

func migrationRequestFor(sql string) policy.ActionRequest {
	request := migrationRequest()
	request.Arguments = []byte(fmt.Sprintf(
		`{"table":"public.users","cycle":1,"sql":%q}`, sql))
	return request
}
