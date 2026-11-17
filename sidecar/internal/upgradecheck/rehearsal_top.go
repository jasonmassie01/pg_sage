package upgradecheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/workload"
)

// ErrNoStatementTexts: production has no pg_stat_statements to read, so a
// rehearsal has no statements to compare.
var ErrNoStatementTexts = errors.New("upgrade rehearsal: pg_stat_statements unavailable " +
	"on production")

// topScanLimit bounds how many pg_stat_statements rows, by total time,
// are scanned for the top statements.
const topScanLimit = 1000

// statementKeywords are the leading keywords of the statements a
// rehearsal lists: reads are compared, writes are listed as skipped, and
// utility statements (BEGIN, SET, VACUUM) are not queries and are left out.
var statementKeywords = map[string]bool{"SELECT": true, "WITH": true, "VALUES": true,
	"TABLE": true, "INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true}

// PostgresTopQueries reads this database's top statements by total
// execution time from production's pg_stat_statements, in a bounded READ
// ONLY transaction. Only their texts leave production.
func PostgresTopQueries(pool *pgxpool.Pool) TopQuerySource {
	return func(ctx context.Context, limit int) ([]TopQuery, error) {
		if pool == nil {
			return nil, ErrNoPool
		}
		if limit <= 0 || limit > MaxRehearsalQueries {
			limit = MaxRehearsalQueries
		}
		tx, err := catalogread.Begin(ctx, pool, catalogread.Default())
		if err != nil {
			return nil, fmt.Errorf("read production's top statements: %w", err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		rel, err := extschema.StatementsRel(ctx, tx, "pg_stat_statements")
		if errors.Is(err, extschema.ErrExtensionAbsent) {
			return nil, fmt.Errorf("%w: %w", ErrNoStatementTexts, err)
		}
		if err != nil {
			return nil, fmt.Errorf("find pg_stat_statements on production: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT queryid, query FROM `+rel+`
			WHERE dbid = (SELECT oid FROM pg_catalog.pg_database
			              WHERE datname = current_database())
			  AND queryid IS NOT NULL AND query IS NOT NULL
			ORDER BY total_exec_time DESC, queryid LIMIT $1`, topScanLimit)
		if err != nil {
			return nil, topError(err)
		}
		defer rows.Close()
		var out []TopQuery
		for rows.Next() && len(out) < limit {
			var q TopQuery
			if err := rows.Scan(&q.QueryID, &q.Text); err != nil {
				return nil, fmt.Errorf("scan a production statement: %w", err)
			}
			if isQuery(q.Text) {
				out = append(out, q)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, topError(err)
		}
		return out, nil
	}
}

// isQuery reports a statement whose leading keyword is a query's.
func isQuery(text string) bool {
	body := strings.TrimLeftFunc(workload.BlankLiteralsAndComments(text), func(r rune) bool {
		return unicode.IsSpace(r) || r == '('
	})
	end := strings.IndexFunc(body, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		end = len(body)
	}
	return statementKeywords[strings.ToUpper(body[:end])]
}

// topError names a pg_stat_statements that is installed but not loaded
// (object_not_in_prerequisite_state) as unavailable.
func topError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55000" {
		return fmt.Errorf("%w: %w", ErrNoStatementTexts, err)
	}
	return fmt.Errorf("read pg_stat_statements on production: %w", err)
}
