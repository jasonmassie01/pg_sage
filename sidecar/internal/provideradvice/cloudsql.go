package provideradvice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cloud SQL and AlloyDB index advisor: the google_db_advisor extension's
// google_db_advisor_recommended_indexes view, read with one bounded
// read-only transaction. A view of any other column shape is reported as
// unknown and not parsed.

const (
	advisorView = "google_db_advisor_recommended_indexes"
	// maxCloudSQLRows bounds one read of the view.
	maxCloudSQLRows = 100
)

// findAdvisorViewSQL names the schema holding the advisor view (public
// first), wherever the extension was installed.
const findAdvisorViewSQL = `SELECT n.nspname FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = $1 AND c.relkind IN ('v', 'm', 'r')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY n.nspname <> 'public', n.nspname LIMIT 1`

const advisorColumnsSQL = `SELECT column_name::text FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`

// CloudSQLSource reads the advisor view of one Cloud SQL or AlloyDB
// database through the monitored database's pool.
type CloudSQLSource struct {
	provider string
	pool     *pgxpool.Pool
	now      func() time.Time
}

// NewCloudSQLSource reads the view for provider cloud-sql or alloydb.
func NewCloudSQLSource(provider string, pool *pgxpool.Pool) (*CloudSQLSource, error) {
	if provider != "cloud-sql" && provider != "alloydb" {
		return nil, fmt.Errorf("the index advisor view is Cloud SQL and AlloyDB only, not %q",
			provider)
	}
	if pool == nil {
		return nil, errors.New("the index advisor view needs the database pool")
	}
	return &CloudSQLSource{provider: provider, pool: pool, now: time.Now}, nil
}

// Provider is the canonical provider name.
func (s *CloudSQLSource) Provider() string { return s.provider }

// Fetch reads at most maxCloudSQLRows recommendations; an absent view is
// no advice.
func (s *CloudSQLSource) Fetch(ctx context.Context) ([]Recommendation, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("index advisor view: begin read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only: nothing to keep
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '5s'; "+
		"SET LOCAL lock_timeout = '1s'"); err != nil {
		return nil, fmt.Errorf("index advisor view: bound the read: %w", err)
	}
	var schema string
	err = tx.QueryRow(ctx, findAdvisorViewSQL, advisorView).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("index advisor view: locate %s: %w", advisorView, err)
	}
	cols, err := advisorColumns(ctx, tx, schema)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(cols, "index") {
		return []Recommendation{s.unknownShape(cols)}, nil
	}
	return s.readView(ctx, tx, schema, slices.Contains(cols, "estimated_storage_size_in_mb"))
}

func advisorColumns(ctx context.Context, tx pgx.Tx, schema string) ([]string, error) {
	rows, err := tx.Query(ctx, advisorColumnsSQL, schema, advisorView)
	if err != nil {
		return nil, fmt.Errorf("index advisor view: read its columns: %w", err)
	}
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("index advisor view: read its columns: %w", err)
	}
	return cols, nil
}

func (s *CloudSQLSource) unknownShape(cols []string) Recommendation {
	return Recommendation{Provider: s.provider, ProviderID: "view-shape", Kind: KindUnknown,
		Reason: fmt.Sprintf("%s has columns (%s), not the known (index, "+
			"estimated_storage_size_in_mb); it is not parsed", advisorView,
			strings.Join(cols, ", ")),
		ObservedAt: s.now().UTC()}
}

func (s *CloudSQLSource) readView(ctx context.Context, tx pgx.Tx, schema string,
	sized bool) ([]Recommendation, error) {
	size := "NULL::text"
	if sized {
		size = "estimated_storage_size_in_mb::text"
	}
	view := pgx.Identifier{schema, advisorView}.Sanitize()
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT "index"::text, %s FROM %s LIMIT %d`,
		size, view, maxCloudSQLRows))
	if err != nil {
		return nil, fmt.Errorf("index advisor view: read %s: %w", view, err)
	}
	now := s.now().UTC()
	var out []Recommendation
	for rows.Next() {
		var ddl, mb *string
		if err := rows.Scan(&ddl, &mb); err != nil {
			rows.Close()
			return nil, fmt.Errorf("index advisor view: scan %s: %w", view, err)
		}
		out = append(out, s.recommendation(deref(ddl), deref(mb), now))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index advisor view: read %s: %w", view, err)
	}
	return out, nil
}

func (s *CloudSQLSource) recommendation(ddl, mb string, now time.Time) Recommendation {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(ddl), " ")))
	rec := Recommendation{Provider: s.provider, Kind: KindCreateIndex, DDL: ddl,
		ProviderID: "index:" + hex.EncodeToString(sum[:8]), ObservedAt: now}
	if _, table, ok := parseCreate(ddl); ok {
		rec.Object = table
	}
	if mb != "" {
		rec.Reason = "estimated storage " + mb + " MB"
	}
	return rec
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
