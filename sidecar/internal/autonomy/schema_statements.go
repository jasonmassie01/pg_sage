package autonomy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/workload"
)

// The schema guard reads pg_stat_statements once per cycle and indexes the
// statements by the schema-qualified names they reference (dogfood lifeos:
// one ILIKE query per foreign key per cycle, each re-reading the statement
// texts). A reference is a qualified name (schema.table, quoted or not),
// matched case-insensitively; a prefix such as public.orders_archive is not
// a reference to public.orders.

// maxQueryIDsPerTarget caps the workload evidence attached to one table.
const maxQueryIDsPerTarget = 20

// maxStatements bounds the statements read per cycle (pg_stat_statements.max
// defaults to 5000).
const maxStatements = 10000

type statementRow struct {
	QueryID int64
	Query   string
}

// statementIndex maps lower-cased "schema.table" references to query IDs in
// descending call order. known is false when the statements could not be
// read, which is never evidence that nothing uses a schema; unavailable
// then says why.
type statementIndex struct {
	known       bool
	unavailable error
	targets     map[string][]int64
	schemas     map[string]bool
}

func (i statementIndex) queryIDs(target string) []int64 {
	return i.targets[strings.ToLower(target)]
}

func (i statementIndex) mentionsSchema(schema string) bool {
	return i.schemas[strings.ToLower(schema)]
}

const identifierPattern = `(?:"(?:[^"]|"")+"|[A-Za-z_][A-Za-z0-9_$]*)`

var (
	identifierRE    = regexp.MustCompile(identifierPattern)
	qualifiedNameRE = regexp.MustCompile(identifierPattern +
		`(?:\s*\.\s*` + identifierPattern + `)+`)
)

// qualifiedNames returns every adjacent pair of a dotted name in query,
// lower-cased with quotes removed ("a.b.c" yields a.b and b.c).
func qualifiedNames(query string) []string {
	var names []string
	for _, chain := range qualifiedNameRE.FindAllString(query, -1) {
		parts := identifierRE.FindAllString(chain, -1)
		for i := 0; i+1 < len(parts); i++ {
			names = append(names, normalizeIdentifier(parts[i])+"."+
				normalizeIdentifier(parts[i+1]))
		}
	}
	return names
}

func normalizeIdentifier(identifier string) string {
	if strings.HasPrefix(identifier, `"`) {
		identifier = strings.ReplaceAll(identifier[1:len(identifier)-1], `""`, `"`)
	}
	return strings.ToLower(identifier)
}

// indexStatements builds the index from rows in descending call order.
func indexStatements(rows []statementRow) statementIndex {
	index := statementIndex{known: true, targets: map[string][]int64{},
		schemas: map[string]bool{}}
	for _, row := range rows {
		seen := map[string]bool{}
		for _, name := range qualifiedNames(row.Query) {
			if seen[name] {
				continue
			}
			seen[name] = true
			schema, _, _ := strings.Cut(name, ".")
			index.schemas[schema] = true
			if len(index.targets[name]) < maxQueryIDsPerTarget {
				index.targets[name] = append(index.targets[name], row.QueryID)
			}
		}
	}
	return index
}

// loadStatementIndex reads this database's statements once. A missing or
// unloaded pg_stat_statements means there are no statements to see (known,
// empty); a statement history we may not read is unknown.
func loadStatementIndex(ctx context.Context, pool *pgxpool.Pool) (statementIndex, error) {
	rows, err := pool.Query(ctx, statementsSQL, maxStatements)
	if err != nil {
		return statementsUnavailable(err)
	}
	defer rows.Close()
	statements := make([]statementRow, 0)
	for rows.Next() {
		var row statementRow
		var query *string
		if err := rows.Scan(&row.QueryID, &query); err != nil {
			return statementIndex{}, fmt.Errorf("scan pg_stat_statements: %w", err)
		}
		if query != nil {
			row.Query = *query
			statements = append(statements, row)
		}
	}
	if err := rows.Err(); err != nil {
		return statementsUnavailable(err)
	}
	return indexStatements(statements), nil
}

func statementsUnavailable(err error) (statementIndex, error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42P01", "42883", "55000": // not installed, not loaded
			return indexStatements(nil), nil
		case "42501": // not allowed to read: unknown, and why
			return statementIndex{unavailable: fmt.Errorf("read pg_stat_statements: %w",
				err)}, nil
		}
	}
	return statementIndex{}, fmt.Errorf("read pg_stat_statements: %w", err)
}

// statementsSQL keeps workload statements (internal/workload): pg_sage is
// tracked by pg_stat_statements (perf v1.8.3) and must not cite its own
// reads, nor an EXPLAIN or VACUUM of the table, as related queries.
var statementsSQL = `/* pg_sage */
SELECT queryid::bigint, query FROM pg_stat_statements
WHERE queryid IS NOT NULL
  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND ` + workload.AdviceSQL("query") + `
ORDER BY calls DESC LIMIT $1`
