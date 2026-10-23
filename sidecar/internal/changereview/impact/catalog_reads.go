package impact

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// indexesSQL lists a table's indexes with their scans and the constraints
// that require them; %s is the last_idx_scan expression (PG16+).
const indexesSQL = `/* pg_sage change_review_indexes v1 */
	SELECT ci.relname, pg_catalog.pg_get_indexdef(i.indexrelid), i.indisunique,
	       i.indisprimary, i.indisvalid AND i.indisready, coalesce(s.idx_scan, 0)::int8,
	       %s,
	       coalesce((SELECT array_agg(k.conname::text ORDER BY k.conname)
	                   FROM pg_catalog.pg_constraint k WHERE k.conindid = i.indexrelid),
	                '{}')
	FROM pg_catalog.pg_index i
	JOIN pg_catalog.pg_class ci ON ci.oid = i.indexrelid
	LEFT JOIN pg_catalog.pg_stat_all_indexes s ON s.indexrelid = i.indexrelid
	WHERE i.indrelid = $1::oid ORDER BY ci.relname`

func (r *run) loadIndexes(rel *relation) {
	last := "NULL::timestamptz"
	if r.version >= 160000 {
		last = "s.last_idx_scan"
	}
	rows, err := r.reader.Query(r.ctx, fmt.Sprintf(indexesSQL, last), rel.oid)
	if r.relFailed(err, rel, readIndexes) {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ix index
		if err := rows.Scan(&ix.name, &ix.def, &ix.unique, &ix.primary, &ix.valid, &ix.scans,
			&ix.lastScan, &ix.backing); err != nil {
			r.relFailed(err, rel, readIndexes)
			return
		}
		ix.parsed = analyzer.ParseIndexDef(ix.def)
		rel.indexes = append(rel.indexes, ix)
	}
	r.relFailed(rows.Err(), rel, readIndexes)
}

func (r *run) loadConstraints(rel *relation) {
	rows, err := r.reader.Query(r.ctx, `/* pg_sage change_review_constraints v1 */
		SELECT k.conname, k.contype::text, k.convalidated,
		       coalesce((SELECT array_agg(a.attname::text ORDER BY u.ord)
		         FROM unnest(k.conkey) WITH ORDINALITY u(attnum, ord)
		         JOIN pg_catalog.pg_attribute a ON a.attrelid = k.conrelid
		          AND a.attnum = u.attnum), '{}'),
		       coalesce(fn.nspname || '.' || fc.relname, ''),
		       coalesce((SELECT array_agg(a.attname::text ORDER BY u.ord)
		         FROM unnest(k.confkey) WITH ORDINALITY u(attnum, ord)
		         JOIN pg_catalog.pg_attribute a ON a.attrelid = k.confrelid
		          AND a.attnum = u.attnum), '{}'),
		       coalesce(ix.relname::text, '')
		FROM pg_catalog.pg_constraint k
		LEFT JOIN pg_catalog.pg_class fc ON fc.oid = k.confrelid
		LEFT JOIN pg_catalog.pg_namespace fn ON fn.oid = fc.relnamespace
		LEFT JOIN pg_catalog.pg_class ix ON ix.oid = k.conindid
		WHERE k.conrelid = $1::oid ORDER BY k.conname`, rel.oid)
	if r.relFailed(err, rel, readConstraints) {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c constraint
		if err := rows.Scan(&c.name, &c.typ, &c.validated, &c.columns, &c.refRel,
			&c.refColumns, &c.index); err != nil {
			r.relFailed(err, rel, readConstraints)
			return
		}
		rel.constraints = append(rel.constraints, c)
	}
	r.relFailed(rows.Err(), rel, readConstraints)
}

// loadViews lists the views and rules depending on rel, with the columns
// each reads (pg_depend over pg_rewrite).
func (r *run) loadViews(rel *relation) {
	rows, err := r.reader.Query(r.ctx, `/* pg_sage change_review_views v1 */
		SELECT vn.nspname || '.' || v.relname,
		       coalesce(array_agg(DISTINCT a.attname::text)
		                FILTER (WHERE a.attname IS NOT NULL), '{}')
		FROM pg_catalog.pg_depend d
		JOIN pg_catalog.pg_rewrite w ON w.oid = d.objid
		JOIN pg_catalog.pg_class v ON v.oid = w.ev_class
		JOIN pg_catalog.pg_namespace vn ON vn.oid = v.relnamespace
		LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = d.refobjid
		     AND a.attnum = d.refobjsubid AND d.refobjsubid > 0
		WHERE d.classid = 'pg_catalog.pg_rewrite'::regclass
		  AND d.refclassid = 'pg_catalog.pg_class'::regclass
		  AND d.refobjid = $1::oid AND w.ev_class <> $1::oid
		GROUP BY 1 ORDER BY 1`, rel.oid)
	if r.relFailed(err, rel, readViews) {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var v viewDep
		if err := rows.Scan(&v.name, &v.columns); err != nil {
			r.relFailed(err, rel, readViews)
			return
		}
		rel.views = append(rel.views, v)
	}
	r.relFailed(rows.Err(), rel, readViews)
}

func (r *run) loadPublications(rel *relation) {
	rows, err := r.reader.Query(r.ctx, `/* pg_sage change_review_publications v1 */
		SELECT DISTINCT pubname::text FROM pg_catalog.pg_publication_tables
		WHERE schemaname = $1 AND tablename = $2 ORDER BY 1`, rel.schema, rel.rel)
	if r.relFailed(err, rel, readPubs) {
		return
	}
	pubs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if r.relFailed(err, rel, readPubs) {
		return
	}
	rel.publications = pubs
}

func (r *run) loadInherits(rel *relation) {
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_inherits v1 */
		SELECT count(*)::int FROM pg_catalog.pg_inherits WHERE inhparent = $1::oid`,
		rel.oid).Scan(&rel.inherits)
	r.relFailed(err, rel, readInherits)
}

// nullFraction is pg_stats' null_frac of rel.col; ok is false when the
// statistics are not visible (no ANALYZE yet, or no SELECT privilege).
func (r *run) nullFraction(rel *relation, col string) (float64, bool, error) {
	var frac float64
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_null_frac v1 */
		SELECT null_frac::float8 FROM pg_catalog.pg_stats
		WHERE schemaname = $1 AND tablename = $2 AND attname = $3
		ORDER BY inherited LIMIT 1`, rel.schema, rel.rel, col).Scan(&frac)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return frac, true, nil
}

// indexTable is the table an existing index belongs to ("" when the
// catalog has no such index).
func (r *run) indexTable(s *stmt, idx sqlast.QualifiedName) string {
	var table string
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_index_table v1 */
		SELECT tn.nspname || '.' || t.relname
		FROM pg_catalog.pg_class ci
		JOIN pg_catalog.pg_namespace n ON n.oid = ci.relnamespace
		JOIN pg_catalog.pg_index i ON i.indexrelid = ci.oid
		JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
		JOIN pg_catalog.pg_namespace tn ON tn.oid = t.relnamespace
		WHERE n.nspname = $1 AND ci.relname = $2`, idx.Schema, idx.Name).Scan(&table)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	r.readFailed(err, s, "table of index "+qualified(idx))
	return table
}
