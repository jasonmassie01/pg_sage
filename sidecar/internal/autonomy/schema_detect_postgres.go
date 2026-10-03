package autonomy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// postgresSchemaDetector reads the catalog for schema invariants: three
// catalog queries per cycle, plus one pg_stat_statements read when foreign
// keys need workload evidence, however many tables the database has. The
// structural scan is the costliest and changes only with DDL: it reruns
// when the catalog changed or hourly (structural); a detector without a
// structural cache scans every cycle.
type postgresSchemaDetector struct {
	pool       *pgxpool.Pool
	now        func() time.Time
	structural *structuralCache
}

// newPostgresSchemaDetector is a detector with a structural cache; now is
// its clock (nil is time.Now).
func newPostgresSchemaDetector(pool *pgxpool.Pool, now func() time.Time) postgresSchemaDetector {
	if now == nil {
		now = time.Now
	}
	return postgresSchemaDetector{pool: pool, now: now, structural: &structuralCache{}}
}

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

// detectMissingFKIndexes lists unindexed foreign keys and attaches the
// statements that use each table, from one pg_stat_statements read.
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
		result = append(result, invariant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate missing foreign-key indexes: %w", err)
	}
	rows.Close()
	if len(result) == 0 {
		return result, nil
	}
	statements, err := loadStatementIndex(ctx, d.pool)
	if err != nil {
		return nil, fmt.Errorf("read workload evidence for foreign-key indexes: %w", err)
	}
	for i := range result {
		result[i].QueryIDs = statements.queryIDs(result[i].Target())
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
		var schemaName, tableName, declared, suggested string
		if err := rows.Scan(&schemaName, &tableName, &declared, &suggested); err != nil {
			return nil, fmt.Errorf("scan unbounded append table: %w", err)
		}
		result = append(result, schemaguard.Invariant{
			Kind:   schemaguard.InvariantUnboundedAppend,
			Schema: schemaName, Table: tableName, RetentionColumn: declared,
			RetentionSuggestion: suggested,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unbounded append tables: %w", err)
	}
	return result, nil
}

// scanStructuralPathologies runs the structural catalog scan.
func (d postgresSchemaDetector) scanStructuralPathologies(
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
			Subject: columnName,
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

func scanSchemaInvariant(row interface{ Scan(...any) error }) (schemaguard.Invariant, error) {
	var schemaName, tableName, constraintName string
	var columns []string
	if err := row.Scan(&schemaName, &tableName, &constraintName, &columns); err != nil {
		return schemaguard.Invariant{}, fmt.Errorf("scan schema invariant: %w", err)
	}
	indexName := boundedIdentifier("pg_sage_fk_" + constraintName + "_idx")
	return schemaguard.Invariant{
		Kind:   schemaguard.InvariantMissingFKIndex,
		Schema: schemaName, Table: tableName, Subject: constraintName,
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
