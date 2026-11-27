package facts

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// CatalogResolver resolves references from pg_class, pg_index and the
// partition tree, with bounded, read-only catalog reads.
type CatalogResolver struct{ pool *pgxpool.Pool }

// NewCatalogResolver resolves against the monitored database.
func NewCatalogResolver(pool *pgxpool.Pool) *CatalogResolver {
	return &CatalogResolver{pool: pool}
}

// resolveCatalogSQL reads, for each (schema, name), the relation's kind,
// an index's table, and the root of the relation's (an index: its
// table's) declarative partition tree. pg_partition_root (PG 12+) is NULL
// for a plain table and a legacy inheritance child; the root is returned
// only when it is another relation, so a root never names itself.
const resolveCatalogSQL = `/* pg_sage facts */
SELECT w.n, c.relkind::text, COALESCE(tn.nspname, ''), COALESCE(t.relname, ''),
       COALESCE(rn.nspname, ''), COALESCE(rc.relname, '')
FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS w(s, r, n)
JOIN pg_catalog.pg_namespace ns ON ns.nspname = w.s
JOIN pg_catalog.pg_class c ON c.relnamespace = ns.oid AND c.relname = w.r
LEFT JOIN pg_catalog.pg_index i ON i.indexrelid = c.oid
LEFT JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid = t.relnamespace
LEFT JOIN pg_catalog.pg_class rc
       ON rc.oid = pg_catalog.pg_partition_root(COALESCE(t.oid, c.oid)::regclass)::oid
      AND rc.oid <> COALESCE(t.oid, c.oid)
LEFT JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace`

// ResolveCatalog implements RefResolver: qualified relations, indexes and
// tables found in the catalog are completed (applyRelkind); the rest are
// returned as they were.
func (c *CatalogResolver) ResolveCatalog(ctx context.Context, refs []ObjectRef) (
	[]ObjectRef, error) {
	out := append([]ObjectRef(nil), refs...)
	var schemas, names []string
	var at []int
	for i, r := range out {
		if r.Schema != "" && (r.Kind == KindRelation || r.Kind == KindIndex ||
			r.Kind == KindTable) {
			schemas, names, at = append(schemas, r.Schema), append(names, r.Name), append(at, i)
		}
	}
	if len(at) == 0 {
		return out, nil
	}
	rows, err := catalogread.New(c.pool, catalogread.Default()).Query(ctx,
		resolveCatalogSQL, schemas, names)
	if err != nil {
		return nil, fmt.Errorf("read index tables and partition roots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		var relkind string
		var table, root [2]string
		if err := rows.Scan(&n, &relkind, &table[0], &table[1], &root[0],
			&root[1]); err != nil {
			return nil, fmt.Errorf("scan index table and partition root: %w", err)
		}
		applyRelkind(&out[at[n-1]], relkind, table, root)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read index tables and partition roots: %w", err)
	}
	return out, nil
}

// applyRelkind completes r from the catalog: a relation of unknown kind
// gets its kind, an index without a table its table, and a table or index
// the root of its partition tree. A ref keeps its written kind and table:
// a resolved ref matches every pattern it matched before, so a fact can
// only narrow.
func applyRelkind(r *ObjectRef, relkind string, table, root [2]string) {
	var kind Kind
	switch relkind {
	case "i", "I":
		kind = KindIndex
		if r.TableName == "" {
			r.TableSchema, r.TableName = table[0], table[1]
		}
	case "r", "p", "m", "f":
		kind = KindTable
	default:
		return
	}
	r.Kind = mergeKind(r.Kind, kind)
	r.RootSchema, r.RootName = root[0], root[1]
}

// mergeKind is a ref's kind once the catalog's is known. PostgreSQL runs
// ALTER INDEX <table> RENAME and ALTER TABLE <index> ..., so a ref written
// as the other kind becomes KindRelation and matches both kinds' patterns.
func mergeKind(written, catalog Kind) Kind {
	switch written {
	case KindRelation:
		return catalog
	case KindTable, KindIndex:
		if written != catalog {
			return KindRelation
		}
	}
	return written
}

// resolveCatalog completes refs with resolver, which must answer one ref
// per ref: a short or long answer is an error, never a dropped ref.
func resolveCatalog(ctx context.Context, resolver RefResolver, refs []ObjectRef) (
	[]ObjectRef, error) {
	got, err := resolver.ResolveCatalog(ctx, refs)
	if err != nil {
		return nil, err
	}
	if len(got) != len(refs) {
		return nil, fmt.Errorf("resolver returned %d refs for %d", len(got), len(refs))
	}
	return got, nil
}
