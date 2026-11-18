package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/extschema"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/workload"
)

// The rehearsal's workload (lab-04b): the statements production spends
// the most time on that reference the migrated table, read from
// production's pg_stat_statements in a bounded READ ONLY transaction.
// Only their texts leave production; they are measured on the clone.

// MaxAffectedStatements is how many statements a rehearsal measures.
const MaxAffectedStatements = 20

// workloadScanLimit bounds how many pg_stat_statements rows, by total
// time, are scanned for statements that reference the table.
const workloadScanLimit = 1000

// NoStatementReferences is the inconclusive detail when production runs
// no statement that references the table.
const NoStatementReferences = "no statement references the table"

// ErrNoStatementTexts: production has no pg_stat_statements to read, so
// there is no statement text to measure.
var ErrNoStatementTexts = errors.New("pg_stat_statements unavailable on production")

// AffectedStatement is a production statement that references the table.
type AffectedStatement struct {
	QueryID int64
	Text    string
}

// WorkloadSource reads the statements, most total time first, that
// reference table; at most MaxAffectedStatements are measured.
type WorkloadSource func(context.Context, planpkg.TableFacts) ([]AffectedStatement, error)

// PostgresWorkload reads the workload from the monitored (production)
// database pool connects to.
func PostgresWorkload(pool *pgxpool.Pool) WorkloadSource {
	return func(ctx context.Context, table planpkg.TableFacts) ([]AffectedStatement, error) {
		if pool == nil {
			return nil, errors.New("read the production workload: no pool")
		}
		tx, err := catalogread.Begin(ctx, pool, catalogread.Default())
		if err != nil {
			return nil, fmt.Errorf("read the production workload: %w", err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		rel, err := extschema.StatementsRel(ctx, tx, "pg_stat_statements")
		if errors.Is(err, extschema.ErrExtensionAbsent) {
			return nil, fmt.Errorf("%w: %w", ErrNoStatementTexts, err)
		}
		if err != nil {
			return nil, fmt.Errorf("find pg_stat_statements on production: %w", err)
		}
		known, err := knownSchemas(ctx, tx, table.Name)
		if err != nil {
			return nil, err
		}
		return referencingStatements(ctx, tx, rel, known, table)
	}
}

// knownSchemas maps the table's name to every schema holding a table of
// that name, the catalog workload.ReferencedTables resolves names in.
func knownSchemas(ctx context.Context, tx pgx.Tx, name string) (map[string][]string, error) {
	rows, err := tx.Query(ctx, `SELECT n.nspname FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = $1 AND c.relkind IN ('r', 'p') ORDER BY n.nspname`, name)
	if err != nil {
		return nil, fmt.Errorf("read the schemas holding %s on production: %w", name, err)
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the schemas holding %s on production: %w", name, err)
	}
	return map[string][]string{name: schemas}, nil
}

// referencingStatements scans this database's statements by total time
// and keeps the first MaxAffectedStatements that reference table.
func referencingStatements(ctx context.Context, tx pgx.Tx, rel string,
	known map[string][]string, table planpkg.TableFacts) ([]AffectedStatement, error) {
	rows, err := tx.Query(ctx, `SELECT queryid, query FROM `+rel+`
		WHERE dbid = (SELECT oid FROM pg_catalog.pg_database
		              WHERE datname = current_database())
		  AND queryid IS NOT NULL AND query IS NOT NULL
		ORDER BY total_exec_time DESC, queryid LIMIT $1`, workloadScanLimit)
	if err != nil {
		return nil, statementsError(err)
	}
	defer rows.Close()
	var out []AffectedStatement
	for rows.Next() && len(out) < MaxAffectedStatements {
		var s AffectedStatement
		if err := rows.Scan(&s.QueryID, &s.Text); err != nil {
			return nil, fmt.Errorf("scan a production statement: %w", err)
		}
		if references(s.Text, known, table) {
			out = append(out, s)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, statementsError(err)
	}
	return out, nil
}

// statementsError names a pg_stat_statements that is installed but not
// loaded (object_not_in_prerequisite_state) as unavailable.
func statementsError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55000" {
		return fmt.Errorf("%w: %w", ErrNoStatementTexts, err)
	}
	return fmt.Errorf("read pg_stat_statements on production: %w", err)
}

func references(text string, known map[string][]string, table planpkg.TableFacts) bool {
	for _, rel := range workload.ReferencedTables(text, known) {
		if rel.Schema == table.Schema && rel.Name == table.Name {
			return true
		}
	}
	return false
}
