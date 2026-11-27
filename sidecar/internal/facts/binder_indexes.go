package facts

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// A statement that rebuilds every index of its table (REINDEX TABLE,
// VACUUM FULL, CLUSTER) touches each of those indexes, though its SQL
// names only the table. A do_not_touch fact on one of them must hold it
// (evid-03b), so the binder adds the table's indexes to the refs when such
// a fact is confirmed.

// IndexResolver lists the indexes of qualified tables from the catalog.
type IndexResolver interface {
	TableIndexes(ctx context.Context, tables []ObjectRef) ([]ObjectRef, error)
}

// withTableIndexes adds to refs the indexes of the tables a request
// rebuilds, when a confirmed do_not_touch fact is about an index and the
// resolver can list them. A failed read is an error (the gate fails
// closed): the held index cannot be shown untouched.
func (b *Binder) withTableIndexes(ctx context.Context, facts []Fact, refs []ObjectRef,
	sql string) ([]ObjectRef, error) {
	ir, ok := b.resolver.(IndexResolver)
	if !ok || !holdsAnIndex(facts) || !rebuildsIndexes(scanSQL(sql)) {
		return refs, nil
	}
	var tables []ObjectRef
	for _, r := range refs {
		if r.Kind == KindTable && r.Schema != "" {
			tables = append(tables, r)
		}
	}
	if len(tables) == 0 {
		return refs, nil
	}
	idx, err := ir.TableIndexes(ctx, tables)
	if err != nil {
		return nil, fmt.Errorf("list the indexes of the rebuilt tables: %w", err)
	}
	return mergeRefs(refs, idx), nil
}

func holdsAnIndex(facts []Fact) bool {
	for _, f := range facts {
		if f.Type == TypeDoNotTouch && f.Kind == KindIndex {
			return true
		}
	}
	return false
}

// rebuildsIndexes reports a REINDEX TABLE, a VACUUM FULL or a CLUSTER.
func rebuildsIndexes(toks []token) bool {
	v := verb(toks)
	if v == "CLUSTER" || vacuumFull(v, toks) {
		return true
	}
	if v != "REINDEX" {
		return false
	}
	s := &stmt{toks: toks, i: 1}
	s.skipParens()
	return s.word(s.i) == "TABLE"
}

// tableIndexesSQL lists, for each (schema, table), its indexes.
const tableIndexesSQL = `/* pg_sage facts */
SELECT n.nspname::text, ic.relname::text, w.s, w.r
FROM unnest($1::text[], $2::text[]) AS w(s, r)
JOIN pg_catalog.pg_namespace tn ON tn.nspname = w.s
JOIN pg_catalog.pg_class t ON t.relnamespace = tn.oid AND t.relname = w.r
JOIN pg_catalog.pg_index i ON i.indrelid = t.oid
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN pg_catalog.pg_namespace n ON n.oid = ic.relnamespace
ORDER BY 1, 2`

// TableIndexes implements IndexResolver with one bounded, read-only catalog
// read. Each index comes with its table.
func (c *CatalogResolver) TableIndexes(ctx context.Context, tables []ObjectRef) (
	[]ObjectRef, error) {
	schemas, names := make([]string, 0, len(tables)), make([]string, 0, len(tables))
	for _, t := range tables {
		schemas, names = append(schemas, t.Schema), append(names, t.Name)
	}
	rows, err := catalogread.New(c.pool, catalogread.Default()).Query(ctx,
		tableIndexesSQL, schemas, names)
	if err != nil {
		return nil, fmt.Errorf("read the tables' indexes: %w", err)
	}
	defer rows.Close()
	var out []ObjectRef
	for rows.Next() {
		r := ObjectRef{Kind: KindIndex}
		if err := rows.Scan(&r.Schema, &r.Name, &r.TableSchema, &r.TableName); err != nil {
			return nil, fmt.Errorf("scan a table's index: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the tables' indexes: %w", err)
	}
	return out, nil
}
