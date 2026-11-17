package upgradecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/extschema"
)

// What the post-upgrade plan is built from, read in one bounded READ ONLY
// transaction: the stored brief, the tables and their rights, the indexes
// whose collation version differs now, and the live top statements.

// maxLiveTables bounds the tables read; a longer list is a database-wide
// ANALYZE anyway.
const maxLiveTables = 10000

// maintainExpr says whether the current role may ANALYZE (and, without
// dbOwner, REINDEX) the relation whose oid is rel: the MAINTAIN privilege
// from PostgreSQL 17, before it ownership, a superuser or, for ANALYZE,
// the database owner.
func maintainExpr(major int, rel string, dbOwner bool) string {
	super := `(SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)`
	if major >= 17 {
		return `(pg_catalog.has_table_privilege(` + rel + `, 'MAINTAIN') OR ` + super + `)`
	}
	expr := `(pg_catalog.pg_has_role((SELECT c2.relowner FROM pg_catalog.pg_class c2
		WHERE c2.oid = ` + rel + `), 'USAGE') OR ` + super
	if dbOwner {
		expr += ` OR pg_catalog.pg_has_role((SELECT d.datdba FROM pg_catalog.pg_database d
			WHERE d.datname = current_database()), 'USAGE')`
	}
	return expr + `)`
}

func liveTablesSQL(major int) string {
	return Tag + `SELECT pg_catalog.format('%I.%I', n.nspname, c.relname),
  pg_catalog.pg_relation_size(c.oid), ` + maintainExpr(major, "c.oid", true) + `
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'm') AND c.relpersistence <> 't' AND NOT c.relispartition
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast'
ORDER BY 1 LIMIT ` + strconv.Itoa(maxLiveTables)
}

// readLiveTables lists the user tables (partitions through their parent,
// pg_sage's own schema left out) with their size and the role's rights.
func readLiveTables(ctx context.Context, tx pgx.Tx, major int) ([]LiveTable, error) {
	rows, err := tx.Query(ctx, liveTablesSQL(major))
	if err != nil {
		return nil, fmt.Errorf("read the tables: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LiveTable, error) {
		var t LiveTable
		err := r.Scan(&t.Name, &t.Bytes, &t.CanMaintain)
		return t, err
	})
}

// readChangedIndexes is the brief's reindex query on the upgraded server,
// with the role's right to rebuild each index.
func readChangedIndexes(ctx context.Context, tx pgx.Tx, major int) ([]LiveIndex, error) {
	rows, err := tx.Query(ctx, Tag+`SELECT x.idx, x.tbl, `+
		maintainExpr(major, "pg_catalog.to_regclass(x.tbl)", false)+`
		FROM (`+reindexSQL(major)+`) AS x(idx, tbl, colls, total)`, maxLiveTables)
	if err != nil {
		return nil, fmt.Errorf("read the indexes whose collation version differs: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LiveIndex, error) {
		var ix LiveIndex
		err := r.Scan(&ix.Index, &ix.Table, &ix.CanMaintain)
		return ix, err
	})
}

// loadBrief is the stored brief of database to newMajor, else its latest
// to any target; nil when none is stored.
func loadBrief(ctx context.Context, tx pgx.Tx, database string, newMajor int) (*Brief,
	error) {
	var doc []byte
	err := tx.QueryRow(ctx, storeTag+`SELECT brief FROM sage.upgrade_briefs
		WHERE database = $1 ORDER BY (target_major = $2) DESC, built_at DESC, id DESC
		LIMIT 1`, database, newMajor).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the stored upgrade brief: %w", err)
	}
	var b Brief
	if err := json.Unmarshal(doc, &b); err != nil {
		return nil, fmt.Errorf("decode the stored upgrade brief: %w", err)
	}
	return &b, nil
}

// liveTopQueries are the busiest statements since the upgrade, the
// targets when the brief records none; best effort (none on any error).
func liveTopQueries(ctx context.Context, tx pgx.Tx) []int64 {
	var out []int64
	_ = catalogread.InSavepoint(ctx, tx, func(sp pgx.Tx) error {
		rel, err := extschema.StatementsRel(ctx, sp, "pg_stat_statements")
		if err != nil {
			return err
		}
		stmts, err := readStatementTimes(ctx, sp, rel)
		for _, s := range stmts {
			if len(out) < maxPostUpgradeTargets && isQuery(s.Text) {
				out = append(out, s.QueryID)
			}
		}
		return err
	})
	return out
}

// read gathers the plan's input in one READ ONLY transaction.
func (p *PostUpgrade) read(ctx context.Context, database string, ch VersionChange) (
	postUpgradeInput, error) {
	in := postUpgradeInput{Database: database, DatabaseID: p.opts.DatabaseID, Change: ch}
	tx, err := catalogread.Begin(ctx, p.opts.Pool, catalogread.Timeouts{
		Statement: p.opts.StatementTimeout, Lock: time.Second})
	if err != nil {
		return in, fmt.Errorf("post-upgrade plan: begin the read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var live int
	if err := tx.QueryRow(ctx, versionSQL).Scan(&live); err != nil {
		return in, fmt.Errorf("post-upgrade plan: read the server version: %w", err)
	}
	if in.Brief, err = loadBrief(ctx, tx, database, ch.NewMajor()); err != nil {
		return in, fmt.Errorf("post-upgrade plan: %w", err)
	}
	if in.Tables, err = readLiveTables(ctx, tx, live/10000); err != nil {
		return in, fmt.Errorf("post-upgrade plan: %w", err)
	}
	if in.Reindex, err = readChangedIndexes(ctx, tx, live/10000); err != nil {
		return in, fmt.Errorf("post-upgrade plan: %w", err)
	}
	in.Fallback = liveTopQueries(ctx, tx)
	return in, nil
}

const raiseFindingSQL = `/* pg_sage post_upgrade_finding */
INSERT INTO sage.findings AS f (category, severity, object_type, object_identifier, title,
    detail, recommendation, recommended_sql, rollback_sql, status, last_seen,
    occurrence_count)
VALUES ($1, 'warning', $2, $3, $4, $5, $6, $7, '', 'open', now(), 1)
ON CONFLICT (category, object_identifier) WHERE status = 'open' DO UPDATE SET
    title = EXCLUDED.title, detail = EXCLUDED.detail,
    recommendation = EXCLUDED.recommendation, recommended_sql = EXCLUDED.recommended_sql,
    last_seen = now(), occurrence_count = f.occurrence_count + 1
RETURNING id`

// raiseFinding opens (or refreshes) the finding a step cites, in pg_sage's
// own sage.findings, under bounded timeouts.
func raiseFinding(ctx context.Context, conn *pgxpool.Conn, f postUpgradeFinding) (int64,
	error) {
	detail, err := json.Marshal(f.Detail)
	if err != nil {
		return 0, fmt.Errorf("post-upgrade plan: encode finding %s: %w", f.Ident, err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("post-upgrade plan: raise finding %s: %w", f.Ident, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var id int64
	_, err = tx.Exec(ctx, storeTag+"SET LOCAL statement_timeout = '5s'")
	if err == nil {
		_, err = tx.Exec(ctx, storeTag+"SET LOCAL lock_timeout = '5s'")
	}
	if err == nil {
		err = tx.QueryRow(ctx, raiseFindingSQL, PostUpgradeCategory, f.ObjectType, f.Ident,
			f.Title, detail, f.Recommendation, f.SQL).Scan(&id)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return 0, fmt.Errorf("post-upgrade plan: raise finding %s: %w", f.Ident, err)
	}
	return id, nil
}
