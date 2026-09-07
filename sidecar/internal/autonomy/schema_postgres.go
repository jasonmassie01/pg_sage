package autonomy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

func NewPostgresSchemaGuard(
	pool *pgxpool.Pool, database string, router ProposalRouter,
	recorder *ledger.Service,
) (*schemaguard.Custodian, error) {
	if pool == nil || strings.TrimSpace(database) == "" || router == nil || recorder == nil {
		return nil, fmt.Errorf("PostgreSQL schema guard dependencies are incomplete")
	}
	policyConfig := schemaguard.DefaultPolicy()
	policyConfig.AllowFKIndexApply = true
	policyConfig.AllowRetentionApply = true
	return schemaguard.NewCustodian(
		postgresSchemaDetector{pool}, postgresSchemaContractSource{pool},
		postgresSchemaHistorySource{pool}, schemaRemediationRouter{
			database: database, router: router,
			verifiedIndexes: verifiedRouter(router),
			rehearsal:       structuralRouter(router),
			retention:       &postgresRetentionEnforcer{pool: pool, batchLimit: 1000},
		},
		schemaDecisionRecorder{recorder}, policyConfig,
	), nil
}

type postgresSchemaDetector struct{ pool *pgxpool.Pool }

func (d postgresSchemaDetector) Detect(ctx context.Context) ([]schemaguard.Invariant, error) {
	fkItems, err := d.detectMissingFKIndexes(ctx)
	if err != nil {
		return nil, err
	}
	appendItems, err := d.detectUnboundedAppend(ctx)
	if err != nil {
		return nil, err
	}
	structuralItems, err := d.detectStructuralPathologies(ctx)
	if err != nil {
		return nil, err
	}
	result := append(fkItems, appendItems...)
	return append(result, structuralItems...), nil
}

func (d postgresSchemaDetector) detectMissingFKIndexes(
	ctx context.Context,
) ([]schemaguard.Invariant, error) {
	rows, err := d.pool.Query(ctx, missingFKIndexSQL)
	if err != nil {
		return nil, fmt.Errorf("detect missing foreign-key indexes: %w", err)
	}
	defer rows.Close()
	result := make([]schemaguard.Invariant, 0)
	for rows.Next() {
		invariant, scanErr := scanSchemaInvariant(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		invariant.QueryIDs = d.relatedQueryIDs(ctx, invariant.Schema, invariant.Table)
		result = append(result, invariant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate missing foreign-key indexes: %w", err)
	}
	return result, nil
}

func (d postgresSchemaDetector) detectUnboundedAppend(
	ctx context.Context,
) ([]schemaguard.Invariant, error) {
	rows, err := d.pool.Query(ctx, unboundedAppendSQL)
	if err != nil {
		return nil, fmt.Errorf("detect unbounded append tables: %w", err)
	}
	defer rows.Close()
	result := make([]schemaguard.Invariant, 0)
	for rows.Next() {
		var schemaName, tableName, retentionColumn string
		if err := rows.Scan(&schemaName, &tableName, &retentionColumn); err != nil {
			return nil, fmt.Errorf("scan unbounded append table: %w", err)
		}
		result = append(result, schemaguard.Invariant{
			Kind:   schemaguard.InvariantUnboundedAppend,
			Schema: schemaName, Table: tableName, RetentionColumn: retentionColumn,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unbounded append tables: %w", err)
	}
	return result, nil
}

func (d postgresSchemaDetector) detectStructuralPathologies(
	ctx context.Context,
) ([]schemaguard.Invariant, error) {
	rows, err := d.pool.Query(ctx, structuralPathologySQL)
	if err != nil {
		return nil, fmt.Errorf("detect structural schema pathologies: %w", err)
	}
	defer rows.Close()
	result := make([]schemaguard.Invariant, 0)
	for rows.Next() {
		var schemaName, tableName, columnName, kind string
		if err := rows.Scan(&schemaName, &tableName, &columnName, &kind); err != nil {
			return nil, fmt.Errorf("scan structural schema pathology: %w", err)
		}
		invariant := schemaguard.Invariant{
			Kind: schemaguard.InvariantKind(kind), Schema: schemaName, Table: tableName,
		}
		if kind == string(schemaguard.InvariantTypeTightening) {
			invariant.ProposedSQL = typeTighteningProposal(schemaName, tableName, columnName)
		}
		result = append(result, invariant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate structural schema pathologies: %w", err)
	}
	return result, nil
}

func (d postgresSchemaDetector) relatedQueryIDs(
	ctx context.Context, schemaName, tableName string,
) []int64 {
	rows, err := d.pool.Query(ctx, `SELECT queryid::bigint FROM pg_stat_statements
		WHERE queryid IS NOT NULL AND query ILIKE '%' || $1 || '%'
		ORDER BY calls DESC LIMIT 20`, schemaName+"."+tableName)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]int64, 0)
	for rows.Next() {
		var queryID int64
		if rows.Scan(&queryID) == nil {
			result = append(result, queryID)
		}
	}
	return result
}

func scanSchemaInvariant(row interface{ Scan(...any) error }) (schemaguard.Invariant, error) {
	var schemaName, tableName, constraintName string
	var columns []string
	if err := row.Scan(&schemaName, &tableName, &constraintName, &columns); err != nil {
		return schemaguard.Invariant{}, fmt.Errorf("scan schema invariant: %w", err)
	}
	indexName := boundedIdentifier("pg_sage_fk_" + constraintName + "_idx")
	return schemaguard.Invariant{
		Kind:   schemaguard.InvariantMissingFKIndex,
		Schema: schemaName, Table: tableName,
		ProposedSQL: "CREATE INDEX CONCURRENTLY " + quoteIdentifier(indexName) +
			" ON " + quoteIdentifier(schemaName) + "." + quoteIdentifier(tableName) +
			" (" + quoteIdentifiers(columns) + ")",
		RollbackSQL: "DROP INDEX CONCURRENTLY " + quoteIdentifier(schemaName) + "." +
			quoteIdentifier(indexName),
	}, nil
}

func typeTighteningProposal(schemaName, tableName, columnName string) string {
	return "ALTER TABLE " + quoteIdentifier(schemaName) + "." + quoteIdentifier(tableName) +
		" ALTER COLUMN " + quoteIdentifier(columnName) +
		" TYPE bigint USING " + quoteIdentifier(columnName) + "::bigint"
}

type postgresSchemaContractSource struct{ pool *pgxpool.Pool }

func (s postgresSchemaContractSource) Contract(
	ctx context.Context, invariant schemaguard.Invariant,
) (schemaguard.TableContract, error) {
	var contract schemaguard.TableContract
	var retentionSeconds *float64
	var exemptions []byte
	err := s.pool.QueryRow(ctx, tableContractSQL, invariant.Schema, invariant.Table).
		Scan(&contract.AppendOnly, &retentionSeconds, &contract.ExpectedPrimaryKey,
			&exemptions)
	if errors.Is(err, pgx.ErrNoRows) {
		return contract, nil
	}
	if err != nil {
		return contract, fmt.Errorf("read table contract: %w", err)
	}
	if retentionSeconds != nil {
		contract.RetentionWindow = time.Duration(*retentionSeconds * float64(time.Second))
	}
	if err := json.Unmarshal(exemptions, &contract.Exemptions); err != nil {
		return contract, fmt.Errorf("decode table contract exemptions: %w", err)
	}
	return contract, nil
}

type postgresSchemaHistorySource struct{ pool *pgxpool.Pool }

func (s postgresSchemaHistorySource) History(
	ctx context.Context, invariant schemaguard.Invariant,
) (schemaguard.History, error) {
	var history schemaguard.History
	target, err := json.Marshal([]string{invariant.Schema + "." + invariant.Table})
	if err != nil {
		return history, fmt.Errorf("encode schema history target: %w", err)
	}
	err = s.pool.QueryRow(ctx, schemaHistorySQL, string(invariant.Kind), target).
		Scan(&history.SuccessfulRetentionDryRuns, &history.ExternalReversions)
	if err != nil {
		return history, fmt.Errorf("read schema remediation history: %w", err)
	}
	return history, nil
}

type schemaRemediationRouter struct {
	database        string
	router          ProposalRouter
	verifiedIndexes VerifiedIndexRouter
	rehearsal       StructuralRehearsalRouter
	retention       *postgresRetentionEnforcer
}

func (r schemaRemediationRouter) Route(
	ctx context.Context, item schemaguard.Remediation,
) error {
	proposal := Proposal{
		Database: r.database, Feature: string(policy.ChangeFKIndex),
		SQL:           item.Invariant.ProposedSQL,
		TargetObjects: []string{item.Invariant.Schema + "." + item.Invariant.Table},
	}
	switch item.Decision.Route {
	case schemaguard.RouteRetention:
		if r.retention == nil {
			return fmt.Errorf("retention enforcement is unavailable")
		}
		return r.retention.Apply(ctx, item)
	case schemaguard.RouteVerifyIndex:
		if r.verifiedIndexes == nil {
			return fmt.Errorf("verified index lifecycle is unavailable")
		}
		return r.verifiedIndexes.RouteVerifiedIndex(
			ctx, proposal, item.Invariant.RollbackSQL, item.Invariant.QueryIDs,
		)
	case schemaguard.RouteCloneRehearsal:
		if r.rehearsal == nil || strings.TrimSpace(proposal.SQL) == "" {
			return nil
		}
		proposal.Feature = string(policy.ChangeOnlineMigration)
		return r.rehearsal.RouteStructuralRehearsal(ctx, proposal)
	case "":
		if strings.TrimSpace(proposal.SQL) == "" {
			return fmt.Errorf("schema remediation SQL is empty")
		}
		return r.router.Route(ctx, proposal)
	}
	return fmt.Errorf("unsupported schema remediation route %q", item.Decision.Route)
}

func verifiedRouter(router ProposalRouter) VerifiedIndexRouter {
	verified, _ := router.(VerifiedIndexRouter)
	return verified
}

func structuralRouter(router ProposalRouter) StructuralRehearsalRouter {
	rehearsal, _ := router.(StructuralRehearsalRouter)
	return rehearsal
}

type schemaDecisionRecorder struct{ ledger *ledger.Service }

func (r schemaDecisionRecorder) Record(
	ctx context.Context, item schemaguard.Remediation,
) error {
	_, err := r.ledger.RecordDecision(ctx, ledger.DecisionInput{
		Feature: "schema_guard", Intent: string(item.Invariant.Kind),
		Evidence: map[string]any{
			"route": item.Decision.Route, "disposition": item.Decision.Disposition,
		},
		ProposedSQL: item.Invariant.ProposedSQL,
		Verdict:     schemaLedgerVerdict(item.Decision.Disposition),
		Reason:      schemaDecisionReason(item.Decision),
		RiskTier:    "moderate", PolicyVersion: 1,
		TargetObjects: []string{item.Invariant.Schema + "." + item.Invariant.Table},
	})
	return err
}

func schemaDecisionReason(decision schemaguard.Decision) string {
	if strings.TrimSpace(decision.Reason) != "" {
		return decision.Reason
	}
	return "schema remediation planned; standing policy authorization pending"
}

func schemaLedgerVerdict(disposition schemaguard.Disposition) ledger.Verdict {
	if disposition == schemaguard.DispositionPark {
		return ledger.VerdictPark
	}
	return ledger.VerdictObserveOnly
}

func quoteIdentifiers(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quoteIdentifier(value))
	}
	return strings.Join(quoted, ", ")
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func boundedIdentifier(value string) string {
	if len(value) <= 63 {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	prefix := value[:54]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + fmt.Sprintf("_%x", digest[:4])
}

const tableContractSQL = `SELECT append_only,
EXTRACT(epoch FROM retention_interval), COALESCE(expected_pk,''), exemptions
FROM sage.table_contract WHERE schema_name=$1 AND table_name=$2
ORDER BY updated_at DESC LIMIT 1`

const schemaHistorySQL = `SELECT
count(*) FILTER (WHERE evidence->>'disposition'='dry_run'),
count(*) FILTER (WHERE evidence->>'external_reversion'='true')
FROM sage.decision WHERE feature='schema_guard' AND intent=$1
AND target_objects @> $2::jsonb`

const unboundedAppendSQL = `/* pg_sage */
SELECT tc.schema_name, tc.table_name, COALESCE(retention_column.attname,'')
FROM sage.table_contract tc
JOIN pg_namespace ns ON ns.nspname=tc.schema_name
JOIN pg_class tbl ON tbl.relnamespace=ns.oid AND tbl.relname=tc.table_name
LEFT JOIN LATERAL (
    SELECT att.attname
    FROM pg_attribute att
    JOIN pg_type typ ON typ.oid=att.atttypid
    WHERE att.attrelid=tbl.oid AND att.attnum>0 AND NOT att.attisdropped
      AND typ.typname IN ('timestamp','timestamptz','date')
    ORDER BY CASE att.attname WHEN 'created_at' THEN 0 WHEN 'occurred_at' THEN 1
             WHEN 'updated_at' THEN 2 ELSE 3 END, att.attnum
    LIMIT 1
) retention_column ON true
WHERE tc.append_only AND tc.retention_interval IS NOT NULL
  AND tbl.relkind IN ('r','p')
ORDER BY tc.schema_name, tc.table_name`

const structuralPathologySQL = `/* pg_sage */
WITH columns AS (
    SELECT ns.nspname AS schema_name, tbl.relname AS table_name,
           att.attname AS column_name, typ.typname AS type_name,
           count(*) OVER (PARTITION BY tbl.oid) AS column_count,
           count(*) FILTER (WHERE typ.typname IN ('text','varchar'))
             OVER (PARTITION BY tbl.oid) AS text_count
    FROM pg_class tbl
    JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
    JOIN pg_attribute att ON att.attrelid=tbl.oid
      AND att.attnum>0 AND NOT att.attisdropped
    JOIN pg_type typ ON typ.oid=att.atttypid
    WHERE tbl.relkind IN ('r','p')
      AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')
)
SELECT DISTINCT schema_name, table_name, '' AS column_name, 'everything_text' AS kind
FROM columns WHERE column_count>=3 AND text_count=column_count
UNION ALL
SELECT schema_name, table_name, column_name, 'type_tightening' AS kind
FROM columns WHERE type_name IN ('text','varchar')
  AND (column_name='count_text' OR column_name ~ '(_id|_count|_number)$')
ORDER BY 1,2,4,3`

const missingFKIndexSQL = `/* pg_sage */
SELECT ns.nspname, tbl.relname, con.conname,
       array_agg(att.attname::text ORDER BY keys.ordinality)
FROM pg_constraint con
JOIN pg_class tbl ON tbl.oid=con.conrelid
JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
JOIN unnest(con.conkey) WITH ORDINALITY keys(attnum, ordinality) ON true
JOIN pg_attribute att ON att.attrelid=con.conrelid AND att.attnum=keys.attnum
WHERE con.contype='f'
  AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')
  AND NOT EXISTS (
      SELECT 1 FROM pg_index idx
      WHERE idx.indrelid=con.conrelid AND idx.indisvalid
        AND con.conkey <@ idx.indkey::smallint[])
GROUP BY ns.nspname, tbl.relname, con.conname
ORDER BY ns.nspname, tbl.relname, con.conname`
