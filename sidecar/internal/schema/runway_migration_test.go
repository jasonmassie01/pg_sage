package schema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sage SRE M6 runways: sage.runway_samples holds the series the runway
// monitor samples on the collector tick (XID and multixact counters, WAL
// position, retained WAL per slot, database and disk usage, sequences).
// The migration is idempotent and its checks refuse malformed rows.
func TestRunwayMigration_SamplesTable(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	cols := runwaySampleColumns(t, ctx, pool)
	want := "id:bigint,kind:text,subject:text,epoch:text," +
		"sampled_at:timestamp with time zone,value:double precision," +
		"counter:double precision,limit_value:double precision"
	if strings.Join(cols, ",") != want {
		t.Fatalf("columns = %v\nwant %s", cols, want)
	}
	var indexed bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE schemaname = 'sage' AND tablename = 'runway_samples'
		  AND indexdef LIKE '%(kind, subject, sampled_at%')`).Scan(&indexed); err != nil ||
		!indexed {
		t.Fatalf("series index missing (%v)", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, value, counter, limit_value)
		VALUES ('xid', 'cluster', 'e1', 1, 2, NULL) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("valid sample refused: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.runway_samples WHERE id = $1", id)
	})
	for name, stmt := range map[string]string{
		"empty subject": `INSERT INTO sage.runway_samples (kind, subject, epoch, value)
			VALUES ('xid', '', 'e1', 1)`,
		"long kind": `INSERT INTO sage.runway_samples (kind, subject, epoch, value)
			VALUES (repeat('k', 33), 'cluster', 'e1', 1)`,
		"empty epoch": `INSERT INTO sage.runway_samples (kind, subject, epoch, value)
			VALUES ('xid', 'cluster', '', 1)`,
		"long subject": `INSERT INTO sage.runway_samples (kind, subject, epoch, value)
			VALUES ('xid', repeat('s', 257), 'e1', 1)`,
		"NaN value": `INSERT INTO sage.runway_samples (kind, subject, epoch, value)
			VALUES ('xid', 'cluster', 'e1', 'NaN')`,
	} {
		_, err := pool.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: err = %v, want a check violation", name, err)
		}
	}
}

// runwaySampleColumns lists sage.runway_samples' columns as name:type.
func runwaySampleColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT column_name || ':' || data_type
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'runway_samples'
		ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns: %v", err)
	}
	return cols
}
