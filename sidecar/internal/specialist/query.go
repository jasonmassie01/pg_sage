package specialist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The caller's optional statement scope (contract revision 1.1.0):
// query_id is a pg_stat_statements queryid (a non-zero signed 64-bit
// integer, sent as a JSON string or an integer and always read exactly);
// query_hash is the hex SHA-256 of the statement's normalized text exactly
// as pg_stat_statements shows it. Both are validated strictly and only
// ever reach SQL as parameters.

// QueryID is a caller's queryid in canonical decimal form ("" when absent).
type QueryID string

var (
	queryHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errQueryID       = errors.New("query_id must be a non-zero signed 64-bit integer " +
		"(a pg_stat_statements queryid) in canonical decimal form")
)

// maxQueryIDBytes bounds the decimal text (sign plus 19 digits).
const maxQueryIDBytes = 20

// UnmarshalJSON reads a JSON string or integer exactly (never through a
// float); null is absent.
func (q *QueryID) UnmarshalJSON(raw []byte) error {
	switch {
	case string(raw) == "null":
		*q = ""
		return nil
	case len(raw) > 0 && raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return errQueryID
		}
		*q = QueryID(s)
	default:
		*q = QueryID(raw)
	}
	_, err := q.Int64()
	return err
}

// Int64 is the queryid; an error unless it is canonical, in range and
// non-zero.
func (q QueryID) Int64() (int64, error) {
	s := string(q)
	if s == "" || len(s) > maxQueryIDBytes {
		return 0, errQueryID
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 || strconv.FormatInt(n, 10) != s {
		return 0, errQueryID
	}
	return n, nil
}

// validateQuery checks the optional statement scope.
func (r *OpenRequest) validateQuery() error {
	if r.QueryID != "" {
		if _, err := r.QueryID.Int64(); err != nil {
			return invalidf("%v", err)
		}
	}
	if r.QueryHash != "" && !queryHashPattern.MatchString(r.QueryHash) {
		return invalidf("query_hash must be 64 lowercase hex characters (the SHA-256 " +
			"of the statement text as pg_stat_statements shows it)")
	}
	return nil
}

// queryHashTimeout bounds one resolution against pg_stat_statements.
const queryHashTimeout = 3 * time.Second

const pssSchemaSQL = `/* pg_sage specialist:query_hash */ SELECT COALESCE((
	SELECT n.nspname FROM pg_catalog.pg_extension e
	JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
	WHERE e.extname = 'pg_stat_statements'), '')`

const resolveHashSQL = `/* pg_sage specialist:query_hash */
SELECT DISTINCT s.queryid FROM %s.pg_stat_statements(true) s
WHERE s.dbid = (SELECT d.oid FROM pg_catalog.pg_database d
                WHERE d.datname = pg_catalog.current_database())
  AND s.queryid IS NOT NULL
  AND pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(s.query, 'UTF8')),
      'hex') = $1
ORDER BY 1 LIMIT 3`

// ResolveQueryHash returns the queryids (at most 3) of the current
// database whose normalized text hashes to hash, in a read-only,
// time-bounded transaction. pg_stat_statements missing is ErrUnavailable.
func ResolveQueryHash(ctx context.Context, pool *pgxpool.Pool, hash string) ([]int64,
	error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: no connection to the database", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, queryHashTimeout)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin the query_hash read: %w", err)
	}
	defer rollback(ctx, tx)
	var schema string
	if err := tx.QueryRow(ctx, pssSchemaSQL).Scan(&schema); err != nil {
		return nil, fmt.Errorf("find pg_stat_statements: %w", err)
	}
	if schema == "" {
		return nil, fmt.Errorf("%w: pg_stat_statements is not installed in this database, "+
			"so query_hash cannot be resolved; pass query_id", ErrUnavailable)
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(resolveHashSQL,
		pgx.Identifier{schema}.Sanitize()), hash)
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	return ids, nil
}

// rollback ends a read-only transaction (nothing to undo); a failure means
// the connection is gone, which the pool discards, and is logged.
func rollback(ctx context.Context, tx pgx.Tx) {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.Warn("specialist: ending the read-only query_hash transaction failed",
			"err", err)
	}
}
