package drill

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The invariant queries. Every one is read-only and runs inside a READ
// ONLY transaction with a statement and lock timeout: on the source they
// are the only statements a drill ever sends, and on the clone they never
// write.

const (
	// exactCountTables is how many of the 20 tables read (the smallest) get count(*).
	exactCountTables = 3
	// sampleRows bounds the checksum sample.
	sampleRows = 1000
	// lockTimeoutMS bounds every lock an invariant query waits for.
	lockTimeoutMS = 1000
)

// tableInfo is one table's catalog entry.
type tableInfo struct {
	Schema, Name string
	Tuples       float64
	Columns      []string
	PK           []string
	CreatedCol   string
}

func (t tableInfo) key() string   { return t.Schema + "." + t.Name }
func (t tableInfo) ident() string { return pgx.Identifier{t.Schema, t.Name}.Sanitize() }

// tablesSelect lists ordinary and partitioned user tables with their
// columns, primary key and created_at-like column. pg_catalog,
// information_schema, the sage schema and extension members are left out.
const tablesSelect = `SELECT n.nspname::text, c.relname::text, c.reltuples::float8,
    ARRAY(SELECT a.attname::text || ' ' || format_type(a.atttypid, a.atttypmod)
          FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0
          AND NOT a.attisdropped ORDER BY a.attnum),
    ARRAY(SELECT a.attname::text FROM pg_index i
          JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
          WHERE i.indrelid = c.oid AND i.indisprimary
          ORDER BY array_position(i.indkey::int2[], a.attnum)),
    COALESCE((SELECT a.attname::text FROM pg_attribute a WHERE a.attrelid = c.oid
          AND a.attnum > 0 AND NOT a.attisdropped
          AND a.atttypid IN ('timestamptz'::regtype, 'timestamp'::regtype)
          AND a.attname IN ('created_at', 'inserted_at', 'created_on', 'creation_time',
                            'created')
          ORDER BY a.attnum LIMIT 1), '')
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass
                  AND d.objid = c.oid AND d.deptype = 'e')`

const sourceTablesSQL = `/* pg_sage restore_drill v1 */ ` + tablesSelect +
	` ORDER BY c.relpages DESC, n.nspname, c.relname LIMIT 20`

const cloneTablesSQL = `/* pg_sage restore_drill v1 */ ` + tablesSelect + `
  AND EXISTS (SELECT 1 FROM unnest($1::text[], $2::text[]) u(s, r)
              WHERE u.s = n.nspname AND u.r = c.relname)`

// sourceFacts is what the source says before the clone is made.
type sourceFacts struct {
	tables []tableInfo
	// sageMarker: sage.snapshots lives in the source database, so its
	// newest collected_at on the clone is the latest committed marker.
	sageMarker bool
}

// readOnly runs fn in a READ ONLY transaction bounded by timeout, in UTC
// with fixed output styles so row text compares across servers.
func readOnly(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration,
	fn func(pgx.Tx) error) error {
	if pool == nil {
		return fmt.Errorf("no connection")
	}
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d;
				SET LOCAL lock_timeout = %d; SET LOCAL TimeZone = 'UTC';
				SET LOCAL DateStyle = 'ISO, MDY'; SET LOCAL IntervalStyle = 'postgres';
				SET LOCAL extra_float_digits = 1`, timeout.Milliseconds(),
				lockTimeoutMS)); err != nil {
				return err
			}
			return fn(tx)
		})
}

func scanTables(rows pgx.Rows) ([]tableInfo, error) {
	defer rows.Close()
	var out []tableInfo
	for rows.Next() {
		var t tableInfo
		if err := rows.Scan(&t.Schema, &t.Name, &t.Tuples, &t.Columns, &t.PK,
			&t.CreatedCol); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// readSource reads the source's 20 largest tables and whether the sage
// schema lives there.
func readSource(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) (
	sourceFacts, error) {
	var facts sourceFacts
	err := readOnly(ctx, pool, timeout, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sourceTablesSQL)
		if err != nil {
			return err
		}
		if facts.tables, err = scanTables(rows); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `/* pg_sage restore_drill v1 */
			SELECT to_regclass('sage.snapshots') IS NOT NULL`).Scan(&facts.sageMarker)
	})
	if err != nil {
		return sourceFacts{}, fmt.Errorf("read source catalog: %w", err)
	}
	return facts, nil
}

// smallest is the n smallest of tables by row estimate (an unanalyzed
// table, -1, sorts as empty), largest first on ties by name.
func smallest(tables []tableInfo, n int) []tableInfo {
	out := append([]tableInfo(nil), tables...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := max(out[i].Tuples, 0), max(out[j].Tuples, 0)
		if a != b {
			return a < b
		}
		return out[i].key() < out[j].key()
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// sample is a checksum of the first sampleRows rows by primary key that
// were created before the recovery point.
type sample struct {
	Rows int64
	MD5  string
}

func sampleSQL(t tableInfo) string {
	order := make([]string, len(t.PK))
	orderT := make([]string, len(t.PK))
	for i, col := range t.PK {
		order[i] = pgx.Identifier{col}.Sanitize()
		orderT[i] = "t." + order[i]
	}
	return fmt.Sprintf(`/* pg_sage restore_drill v1 */
SELECT count(*), md5(COALESCE(string_agg(t::text, '' ORDER BY %s), ''))
FROM (SELECT * FROM %s WHERE %s < $1 ORDER BY %s LIMIT %d) t`,
		strings.Join(orderT, ", "), t.ident(), pgx.Identifier{t.CreatedCol}.Sanitize(),
		strings.Join(order, ", "), sampleRows)
}

// sampled reports whether t gets a sample checksum.
func sampled(t tableInfo) bool { return len(t.PK) > 0 && t.CreatedCol != "" }

// exactFacts is what the source says after the clone is checked: exact
// counts of the smallest tables and the sample checksums.
type exactFacts struct {
	counts map[string]int64
	sums   map[string]sample
}

// readSourceExact counts and samples, on the source, the tables the clone
// had with the same columns.
func readSourceExact(ctx context.Context, pool *pgxpool.Pool, src sourceFacts,
	cl cloneFacts, recoveryPoint time.Time, timeout time.Duration) (exactFacts, error) {
	out := exactFacts{counts: map[string]int64{}, sums: map[string]sample{}}
	err := readOnly(ctx, pool, timeout, func(tx pgx.Tx) error {
		for key := range cl.counts {
			var n int64
			t := cl.tables[key]
			if err := tx.QueryRow(ctx, `/* pg_sage restore_drill v1 */ SELECT count(*) FROM `+
				t.ident()).Scan(&n); err != nil {
				return fmt.Errorf("count %s: %w", key, err)
			}
			out.counts[key] = n
		}
		for _, t := range src.tables {
			if _, ok := cl.sums[t.key()]; !ok {
				continue
			}
			var s sample
			if err := tx.QueryRow(ctx, sampleSQL(t), recoveryPoint).Scan(&s.Rows,
				&s.MD5); err != nil {
				return fmt.Errorf("sample %s: %w", t.key(), err)
			}
			out.sums[t.key()] = s
		}
		return nil
	})
	if err != nil {
		return exactFacts{}, fmt.Errorf("read source counts: %w", err)
	}
	return out, nil
}
