package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// D5: a retention contract names the owner's retention column; pg_sage never
// infers it. No concurrent access tests: declaration is a single upsert and
// each test owns its tables.

func declareContract(
	t *testing.T, store IntentStore, arguments string,
) (any, error) {
	t.Helper()
	executor := NewProductionIntentExecutor(store, plan.NewPlanner())
	return executor.Execute(context.Background(), policy.ActionRequest{
		Contract:  &policy.ActionContract{ActionType: "declare_table_contract"},
		Arguments: json.RawMessage(arguments),
	}, policy.Decision{Verdict: policy.VerdictExecute, EvidenceID: "ev-retention"})
}

func TestDeclareTableContractRejectsIntervalWithoutColumn(t *testing.T) {
	for name, retention := range map[string]string{
		"object without column": `{"interval":"30 days"}`,
		"blank column":          `{"interval":"30 days","column":"  "}`,
		"bare interval string":  `"30 days"`,
	} {
		t.Run(name, func(t *testing.T) {
			store := &recordingIntentStore{}
			_, err := declareContract(t, store, `{"table":"public.events",`+
				`"append_only":true,"retention":`+retention+`}`)
			require.Error(t, err)
			require.Contains(t, err.Error(), "retention.column")
			require.Equal(t, TableContractDeclaration{}, store.contract,
				"a rejected declaration must not reach the store")
		})
	}
}

func TestDeclareTableContractRejectsColumnWithoutInterval(t *testing.T) {
	store := &recordingIntentStore{}
	_, err := declareContract(t, store,
		`{"table":"public.events","retention":{"column":"created_at"}}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "interval")
	require.Equal(t, TableContractDeclaration{}, store.contract)
}

func TestDeclareTableContractPassesDeclaredColumnToStore(t *testing.T) {
	store := &recordingIntentStore{}
	_, err := declareContract(t, store, `{"table":"public.events","append_only":true,`+
		`"retention":{"interval":"30 days","column":" ingested_at "}}`)
	require.NoError(t, err)
	require.Equal(t, "30 days", store.contract.Retention)
	require.Equal(t, "ingested_at", store.contract.RetentionColumn)
}

func TestDeclareTableContractWithoutRetentionNeedsNoColumn(t *testing.T) {
	store := &recordingIntentStore{}
	_, err := declareContract(t, store, `{"table":"public.events","append_only":true}`)
	require.NoError(t, err)
	require.Empty(t, store.contract.Retention)
	require.Empty(t, store.contract.RetentionColumn)
}

func retentionContractTable(t *testing.T, pool *pgxpool.Pool, ddl string) string {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("mcp_retention_%d", time.Now().UnixNano())
	for _, statement := range strings.Split(strings.ReplaceAll(ddl, "{t}", table), ";") {
		_, err := pool.Exec(ctx, statement)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.table_contract WHERE table_name=$1", table)
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table+" CASCADE")
	})
	return table
}

func retentionDeclaration(table, column string) TableContractDeclaration {
	return TableContractDeclaration{
		Schema: "public", Table: table, AppendOnly: true, Retention: "30 days",
		RetentionColumn: column, DeclaredBy: "mcp-agent", EvidenceID: "ev-" + table + column,
	}
}

func TestDeclareTableContractRejectsMissingOrNonTemporalColumn(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	table := retentionContractTable(t, pool, "CREATE TABLE {t} (id bigint, note text, "+
		"old_ts timestamptz, created_at timestamptz);ALTER TABLE {t} DROP COLUMN old_ts")
	for column, want := range map[string]string{
		"note": "timestamptz", "old_ts": "does not exist", "absent": "does not exist",
	} {
		_, err := access.DeclareTableContract(context.Background(),
			retentionDeclaration(table, column))
		require.Error(t, err, "column %s", column)
		require.Contains(t, err.Error(), want)
	}
	var contracts int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*)
		FROM sage.table_contract WHERE table_name=$1`, table).Scan(&contracts))
	require.Equal(t, 0, contracts, "rejected declarations must not be persisted")
}

func TestDeclareTableContractPersistsDeclaredColumn(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	for _, columnType := range []string{"timestamptz", "timestamp", "date"} {
		table := retentionContractTable(t, pool,
			"CREATE TABLE {t} (id bigint, ingested_at "+columnType+")")
		outcome, err := access.DeclareTableContract(context.Background(),
			retentionDeclaration(table, "ingested_at"))
		require.NoError(t, err, "type %s", columnType)
		require.True(t, outcome.Applied)
		require.Empty(t, outcome.Warnings)
		var column string
		require.NoError(t, pool.QueryRow(context.Background(), `SELECT retention_column
			FROM sage.table_contract WHERE table_name=$1`, table).Scan(&column))
		require.Equal(t, "ingested_at", column)
	}
}

func TestDeclareTableContractWarnsWhenColumnIsNotPartitionKey(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	table := retentionContractTable(t, pool, "CREATE TABLE {t} "+
		"(id bigint, created_at timestamptz) PARTITION BY RANGE (id)")
	outcome, err := access.DeclareTableContract(context.Background(),
		retentionDeclaration(table, "created_at"))
	require.NoError(t, err)
	require.True(t, outcome.Applied)
	require.Len(t, outcome.Warnings, 1)
	require.Contains(t, outcome.Warnings[0], "partition key")

	keyed := retentionContractTable(t, pool, "CREATE TABLE {t} "+
		"(id bigint, created_at timestamptz) PARTITION BY RANGE (created_at)")
	outcome, err = access.DeclareTableContract(context.Background(),
		retentionDeclaration(keyed, "created_at"))
	require.NoError(t, err)
	require.Empty(t, outcome.Warnings)
}

// Re-declaring a contract (NULL database_id, a fresh evidence id each time)
// updates the one row for the table instead of adding another.
func TestDeclareTableContractTwiceKeepsOneRow(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()
	table := retentionContractTable(t, pool,
		"CREATE TABLE {t} (id bigint, created_at timestamptz, ingested_at timestamptz)")
	first := retentionDeclaration(table, "created_at")
	_, err := access.DeclareTableContract(ctx, first)
	require.NoError(t, err)
	second := retentionDeclaration(table, "ingested_at")
	second.Retention = "90 days"
	_, err = access.DeclareTableContract(ctx, second)
	require.NoError(t, err)

	var rows int
	var column, interval, evidence string
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), max(retention_column),
		max(retention_interval::text), max(evidence_id)
		FROM sage.table_contract WHERE schema_name='public' AND table_name=$1`, table).
		Scan(&rows, &column, &interval, &evidence))
	require.Equal(t, 1, rows, "re-declaration must not add a second contract row")
	require.Equal(t, "ingested_at", column)
	require.Equal(t, "90 days", interval)
	require.Equal(t, second.EvidenceID, evidence)
}
