package partition

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// convertLockTimeout bounds the wait for the exclusive lock the conversion
// takes; convertStatementTimeout bounds its one scan (validating the
// history partition's bound reads the old table's heap once).
const (
	convertLockTimeout      = 30 * time.Second
	convertStatementTimeout = 10 * time.Minute
)

// tableIndex is one index of the plain table being converted.
type tableIndex struct {
	name, def  string
	unique     bool
	primary    bool
	constraint string // backing constraint; "" for a plain index
}

// grant is one privilege on the plain table, re-granted on the new one.
type grant struct {
	grantee, privilege string
	grantable          bool
}

// convertPlan is what Convert reads before changing anything.
type convertPlan struct {
	cutover   time.Time
	indexes   []tableIndex
	sequences map[string]string // column -> owned sequence
	grants    []grant
	owner     string
}

// Convert turns the plain table t into a table partitioned by UTC day. The
// old table becomes the history partition, holding every row before the
// cutover (the start of tomorrow, UTC), so no row is copied; rows dated
// later move to the default partition. Secondary indexes keep their names
// on the partitioned table, the owned id sequence, defaults and grants
// carry over, and the primary key becomes Key plus the day column (or is
// dropped when Key is nil). It returns false when t is partitioned already.
// Everything happens in one transaction: on any error t is unchanged.
func Convert(ctx context.Context, db DB, t Table) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("partition: convert %s: begin: %w", t.regclass(), err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockedSession(ctx, tx, "sage.partition."+t.Name, convertLockTimeout); err != nil {
		return false, fmt.Errorf("partition: convert %s: %w", t.regclass(), err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'",
		convertStatementTimeout.Milliseconds())); err != nil {
		return false, fmt.Errorf("partition: convert %s: %w", t.regclass(), err)
	}
	if done, err := Partitioned(ctx, tx, t); err != nil || done {
		return false, err
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+t.ident()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return false, fmt.Errorf("partition: convert %s: lock: %w", t.regclass(), err)
	}
	plan, err := readPlan(ctx, tx, t)
	if err != nil {
		return false, fmt.Errorf("partition: convert %s: %w", t.regclass(), err)
	}
	for _, step := range convertSteps(t, plan) {
		if _, err := tx.Exec(ctx, step); err != nil {
			return false, fmt.Errorf("partition: convert %s: %s: %w", t.regclass(),
				firstLine(step), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("partition: convert %s: commit: %w", t.regclass(), err)
	}
	return true, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func readPlan(ctx context.Context, tx pgx.Tx, t Table) (convertPlan, error) {
	p := convertPlan{sequences: map[string]string{}}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now(), pg_catalog.pg_get_userbyid(relowner)::text
		FROM pg_catalog.pg_class WHERE oid = pg_catalog.to_regclass($1)`, t.regclass()).
		Scan(&now, &p.owner); err != nil {
		return p, fmt.Errorf("read owner: %w", err)
	}
	p.cutover = DayStart(now).Add(day)
	var err error
	if p.indexes, err = readIndexes(ctx, tx, t); err != nil {
		return p, err
	}
	if p.sequences, err = readSequences(ctx, tx, t); err != nil {
		return p, err
	}
	p.grants, err = readGrants(ctx, tx, t, p.owner)
	return p, err
}

func readIndexes(ctx context.Context, tx pgx.Tx, t Table) ([]tableIndex, error) {
	rows, err := tx.Query(ctx, `SELECT c.relname::text, pg_catalog.pg_get_indexdef(i.indexrelid),
		i.indisunique, i.indisprimary, COALESCE(con.conname::text, '')
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		LEFT JOIN pg_catalog.pg_constraint con
		  ON con.conindid = i.indexrelid AND con.conrelid = i.indrelid
		WHERE i.indrelid = pg_catalog.to_regclass($1) ORDER BY c.relname`, t.regclass())
	if err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tableIndex, error) {
		var ix tableIndex
		err := r.Scan(&ix.name, &ix.def, &ix.unique, &ix.primary, &ix.constraint)
		return ix, err
	})
	if err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	for _, ix := range out {
		if (ix.unique || ix.constraint != "") && !ix.primary {
			return nil, fmt.Errorf("unique index %s cannot carry over to a table "+
				"partitioned by %s", ix.name, t.Column)
		}
	}
	return out, nil
}

func readSequences(ctx context.Context, tx pgx.Tx, t Table) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT a.attname::text,
		pg_catalog.pg_get_serial_sequence($1, a.attname)
		FROM pg_catalog.pg_attribute a
		WHERE a.attrelid = pg_catalog.to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped
		  AND pg_catalog.pg_get_serial_sequence($1, a.attname) IS NOT NULL`, t.regclass())
	if err != nil {
		return nil, fmt.Errorf("read sequences: %w", err)
	}
	out := map[string]string{}
	for rows.Next() {
		var col, seq string
		if err := rows.Scan(&col, &seq); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read sequences: %w", err)
		}
		out[col] = seq
	}
	rows.Close()
	return out, rows.Err()
}

func readGrants(ctx context.Context, tx pgx.Tx, t Table, owner string) ([]grant, error) {
	rows, err := tx.Query(ctx, `SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC'
		ELSE pg_catalog.pg_get_userbyid(a.grantee)::text END, a.privilege_type, a.is_grantable
		FROM pg_catalog.pg_class c, pg_catalog.aclexplode(c.relacl) a
		WHERE c.oid = pg_catalog.to_regclass($1)`, t.regclass())
	if err != nil {
		return nil, fmt.Errorf("read grants: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (grant, error) {
		var g grant
		err := r.Scan(&g.grantee, &g.privilege, &g.grantable)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("read grants: %w", err)
	}
	out := all[:0]
	for _, g := range all {
		if g.grantee != owner {
			out = append(out, g)
		}
	}
	return out, nil
}
