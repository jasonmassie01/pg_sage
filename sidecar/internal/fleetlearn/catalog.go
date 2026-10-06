package fleetlearn

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReadOptions bound one database's fingerprint read.
type ReadOptions struct {
	Database     string
	Boundary     string
	IncludeNames bool
	MaxTables    int // 0: DefaultMaxTables
	MaxQueries   int // 0: DefaultMaxQueries
}

// Read bounds.
const (
	DefaultMaxTables  = 2000
	DefaultMaxQueries = 200
)

// TableIndex maps "schema.table" (lower case) to its table shape hash. It
// stays in memory: names never leave the database unless opted in.
type TableIndex map[string]string

var errNoPool = errors.New("no connection pool")

// tableColumnsSQL lists user tables (no partitions, no extension members,
// no system or pg_sage schemas) with their column types and attribute numbers.
const tableColumnsSQL = `/* pg_sage */ SELECT c.oid, n.nspname, c.relname,
	array(SELECT format_type(a.atttypid, NULL) FROM pg_catalog.pg_attribute a
		WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum),
	array(SELECT a.attnum::int FROM pg_catalog.pg_attribute a
		WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum)
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
	  AND n.nspname NOT LIKE 'pg\_%'
	  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
		WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid
		  AND d.deptype = 'e')
	ORDER BY c.relpages DESC, c.oid
	LIMIT $1`

const indexShapesSQL = `/* pg_sage */ SELECT i.indrelid, am.amname, i.indisunique,
	i.indpred IS NOT NULL, i.indkey::text
	FROM pg_catalog.pg_index i
	JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
	JOIN pg_catalog.pg_am am ON am.oid = ic.relam
	WHERE i.indrelid = ANY($1)`

type tableShape struct {
	label   string
	hash    string
	ordinal map[int]int // attnum -> 1-based position among live columns
}

// ReadFingerprint reads one database's fingerprint and its table index.
func ReadFingerprint(ctx context.Context, pool *pgxpool.Pool,
	opts ReadOptions) (Fingerprint, TableIndex, error) {
	if pool == nil {
		return Fingerprint{}, nil, fmt.Errorf("fingerprint: %w", errNoPool)
	}
	tables, err := readTables(ctx, pool, opts.MaxTables)
	if err != nil {
		return Fingerprint{}, nil, err
	}
	indexes, err := readIndexes(ctx, pool, tables)
	if err != nil {
		return Fingerprint{}, nil, err
	}
	queries, err := readQueryShapes(ctx, pool, opts.MaxQueries)
	if err != nil {
		return Fingerprint{}, nil, err
	}
	fp := Fingerprint{Database: opts.Database, Boundary: opts.Boundary,
		Indexes: sortedUnique(indexes), Queries: sortedUnique(queries),
		ComputedAt: time.Now().UTC()}
	idx := TableIndex{}
	hashes := make([]string, 0, len(tables))
	for _, t := range tables {
		idx[t.label] = t.hash
		hashes = append(hashes, t.hash)
	}
	fp.Tables = sortedUnique(hashes)
	if opts.IncludeNames && len(tables) > 0 {
		fp.Labels = labelsOf(tables)
	}
	return fp, idx, nil
}

func labelsOf(tables map[uint32]tableShape) map[string]string {
	labels := map[string]string{}
	for _, t := range tables {
		if cur, ok := labels[t.hash]; !ok || t.label < cur {
			labels[t.hash] = t.label
		}
	}
	return labels
}

func readTables(ctx context.Context, pool *pgxpool.Pool,
	max int) (map[uint32]tableShape, error) {
	if max <= 0 {
		max = DefaultMaxTables
	}
	rows, err := pool.Query(ctx, tableColumnsSQL, max)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: read tables: %w", err)
	}
	defer rows.Close()
	out := map[uint32]tableShape{}
	for rows.Next() {
		var oid uint32
		var schema, name string
		var types []string
		var attnums []int
		if err := rows.Scan(&oid, &schema, &name, &types, &attnums); err != nil {
			return nil, fmt.Errorf("fingerprint: scan table: %w", err)
		}
		t := tableShape{label: strings.ToLower(schema + "." + name),
			hash: TableShapeHash(types), ordinal: map[int]int{}}
		for i, n := range attnums {
			t.ordinal[n] = i + 1
		}
		out[oid] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fingerprint: read tables: %w", err)
	}
	return out, nil
}

func readIndexes(ctx context.Context, pool *pgxpool.Pool,
	tables map[uint32]tableShape) ([]string, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	oids := make([]uint32, 0, len(tables))
	for oid := range tables {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool { return oids[i] < oids[j] })
	rows, err := pool.Query(ctx, indexShapesSQL, oids)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: read indexes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel uint32
		var method, indkey string
		var unique, partial bool
		if err := rows.Scan(&rel, &method, &unique, &partial, &indkey); err != nil {
			return nil, fmt.Errorf("fingerprint: scan index: %w", err)
		}
		t := tables[rel]
		out = append(out, IndexShapeHash(t.hash, method, unique, partial,
			keyPositions(indkey, t.ordinal)))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fingerprint: read indexes: %w", err)
	}
	return out, nil
}

// keyPositions maps an indkey ("2 0 3") to live column positions; an
// expression column is 0.
func keyPositions(indkey string, ordinal map[int]int) []int {
	fields := strings.Fields(indkey)
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		out = append(out, ordinal[n])
	}
	return out
}

const pgssSchemaSQL = `/* pg_sage */ SELECT n.nspname FROM pg_catalog.pg_extension e
	JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
	WHERE e.extname = 'pg_stat_statements'`

// readQueryShapes hashes the shapes of the database's most frequent data
// statements. Without pg_stat_statements there are none (not an error).
// The statement text never leaves this function.
func readQueryShapes(ctx context.Context, pool *pgxpool.Pool, max int) ([]string, error) {
	if max <= 0 {
		max = DefaultMaxQueries
	}
	var schema string
	err := pool.QueryRow(ctx, pgssSchemaSQL).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fingerprint: find pg_stat_statements: %w", err)
	}
	sql := `/* pg_sage */ SELECT query FROM ` + pgx.Identifier{schema,
		"pg_stat_statements"}.Sanitize() + ` WHERE dbid = (SELECT oid FROM
		pg_catalog.pg_database WHERE datname = current_database())
		ORDER BY calls DESC LIMIT $1`
	rows, err := pool.Query(ctx, sql, max*4)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: read statements: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() && len(out) < max {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("fingerprint: scan statement: %w", err)
		}
		if h := workloadShape(text); h != "" {
			out = append(out, h)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fingerprint: read statements: %w", err)
	}
	return out, nil
}

// notWorkload matches pg_sage's own statements and catalog reads.
var notWorkload = regexp.MustCompile(
	`(?i)pg_sage|\bsage\s*\.|\bpg_\w+|information_schema`)

// workloadShape is the shape hash of an application data statement; ""
// for pg_sage's own statements, catalog reads and utility statements.
func workloadShape(text string) string {
	if notWorkload.MatchString(text) {
		return ""
	}
	n := NormalizeQuery(text)
	first, _, _ := strings.Cut(n, " ")
	switch first {
	case "select", "insert", "update", "delete", "with":
		return shapeHash("q1:" + n)
	}
	return ""
}

const tableShapeSQL = `/* pg_sage */ SELECT
	array(SELECT format_type(a.atttypid, NULL) FROM pg_catalog.pg_attribute a
		WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum)
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p') AND lower(n.nspname) = $1 AND lower(c.relname) = $2
	LIMIT 1`

// ReadTableShape is the shape hash of one table ("schema.table" or an
// unqualified name in public); "" when it does not exist.
func ReadTableShape(ctx context.Context, pool *pgxpool.Pool, table string) (string, error) {
	if strings.TrimSpace(table) == "" {
		return "", nil
	}
	if pool == nil {
		return "", fmt.Errorf("table shape: %w", errNoPool)
	}
	schema, name, _ := strings.Cut(normalizeTable(table), ".")
	var types []string
	err := pool.QueryRow(ctx, tableShapeSQL, schema, name).Scan(&types)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("table shape: read %s: %w", table, err)
	}
	return TableShapeHash(types), nil
}
