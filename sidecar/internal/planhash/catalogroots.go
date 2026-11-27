package planhash

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxResolvedRelations bounds the names one catalog read resolves.
const MaxResolvedRelations = 256

// The catalog read is read-only, takes no relation locks and is bounded.
const (
	rootsStatementTimeout = "2000ms"
	rootsLockTimeout      = "100ms"
	rootsRollbackTimeout  = 5 * time.Second
)

// Beginner opens a transaction; *pgxpool.Pool and *pgx.Conn satisfy it.
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

var errNoDatabase = errors.New("planhash: no database to resolve partition roots")

// partitionRootsSQL maps each (schema, name) to its partition root and
// the root's leaf count. An empty schema (plans without VERBOSE) matches
// the name in every non-system schema; matches > 1 means ambiguous unless
// plain is true: no candidate is a partition or partitioned, so each one
// is its own root and the name resolves to itself. The leaf count walks pg_inherits instead of pg_partition_tree, which would
// lock every partition. pg_partition_root exists on PostgreSQL 12+.
const partitionRootsSQL = `/* pg_sage planhash:partition_roots v2 */
WITH RECURSIVE n AS (
    SELECT DISTINCT u.nsp, u.rel
    FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[]))
         AS u(nsp, rel)
), m AS (
    SELECT n.nsp, n.rel, c.oid,
           pg_catalog.count(*) OVER (PARTITION BY n.nsp, n.rel) AS matches
    FROM n
    JOIN pg_catalog.pg_class c ON c.relname = n.rel
    JOIN pg_catalog.pg_namespace s ON s.oid = c.relnamespace
    WHERE s.nspname = n.nsp
       OR (n.nsp = '' AND s.nspname NOT LIKE 'pg\_%'
           AND s.nspname <> 'information_schema')
), r AS (
    SELECT m.nsp, m.rel, m.matches, pg_catalog.pg_partition_root(m.oid)::oid AS root
    FROM m
), tree(root, relid, depth) AS (
    SELECT DISTINCT r.root, r.root, 0 FROM r WHERE r.root IS NOT NULL
    UNION ALL
    SELECT t.root, i.inhrelid, t.depth + 1
    FROM tree t JOIN pg_catalog.pg_inherits i ON i.inhparent = t.relid
    WHERE t.depth < 64
), leaves AS (
    SELECT t.root, pg_catalog.count(*) AS n
    FROM tree t JOIN pg_catalog.pg_class c ON c.oid = t.relid
    WHERE c.relkind <> 'p'
    GROUP BY t.root
)
SELECT r.nsp, r.rel, r.matches::int8, rs.nspname::text, rc.relname::text,
       CASE WHEN rc.relkind = 'p' THEN COALESCE(l.n, 0) END::int8,
       pg_catalog.bool_and(r.root IS NULL) OVER (PARTITION BY r.nsp, r.rel) AS plain
FROM r
LEFT JOIN pg_catalog.pg_class rc ON rc.oid = r.root
LEFT JOIN pg_catalog.pg_namespace rs ON rs.oid = rc.relnamespace
LEFT JOIN leaves l ON l.root = r.root`

// catalogRoots is one capture's resolution; a missing name is unresolved.
type catalogRoots map[Relation]Root

func (c catalogRoots) Root(schema, rel string) (Root, error) {
	if r, ok := c[Relation{Schema: schema, Name: rel}]; ok {
		return r, nil
	}
	return Root{}, ErrUnresolved
}

// failedRoots leaves every relation unresolved after a failed read.
type failedRoots struct{ err error }

func (f failedRoots) Root(string, string) (Root, error) { return Root{}, f.err }

// Resolve reads the partition roots of every relation the plans name with
// one bounded, read-only catalog query. It always returns a usable
// resolver: after an error (reported so callers can log it) every
// relation is unresolved and ComputeWith yields a v2u fingerprint. Plans
// that are not valid EXPLAIN JSON contribute no names.
func Resolve(ctx context.Context, db Beginner, plans ...[]byte) (RootResolver, error) {
	var rels []Relation
	seen := map[Relation]bool{}
	for _, plan := range plans {
		root, err := rootNode(plan)
		if err != nil {
			continue
		}
		names, err := relations(root)
		if err != nil {
			continue
		}
		for _, r := range names {
			if !seen[r] {
				seen[r] = true
				rels = append(rels, r)
			}
		}
	}
	switch {
	case len(rels) == 0:
		return catalogRoots{}, nil
	case len(rels) > MaxResolvedRelations:
		err := fmt.Errorf("planhash: plan names %d relations, more than the %d resolved",
			len(rels), MaxResolvedRelations)
		return failedRoots{err}, err
	case db == nil:
		return failedRoots{errNoDatabase}, errNoDatabase
	}
	roots, err := readRoots(ctx, db, rels)
	if err != nil {
		err = fmt.Errorf("planhash: read partition roots of %d relations: %w", len(rels), err)
		return failedRoots{err}, err
	}
	return roots, nil
}

func readRoots(ctx context.Context, db Beginner, rels []Relation) (catalogRoots, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin read-only transaction: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), rootsRollbackTimeout)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('statement_timeout', $1, true),
		pg_catalog.set_config('lock_timeout', $2, true)`,
		rootsStatementTimeout, rootsLockTimeout); err != nil {
		return nil, fmt.Errorf("set read bounds: %w", err)
	}
	nsps, names := make([]string, len(rels)), make([]string, len(rels))
	for i, r := range rels {
		nsps[i], names[i] = r.Schema, r.Name
	}
	rows, err := tx.Query(ctx, partitionRootsSQL, nsps, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := catalogRoots{}
	for rows.Next() {
		var rel Relation
		var matches int64
		var rootSchema, rootName *string
		var leaves *int64
		var plain bool
		if err := rows.Scan(&rel.Schema, &rel.Name, &matches, &rootSchema, &rootName,
			&leaves, &plain); err != nil {
			return nil, fmt.Errorf("scan partition root: %w", err)
		}
		out.add(rel, matches, plain, rootSchema, rootName, leaves)
	}
	return out, rows.Err()
}

// add records one match. A relation that is not a partition resolves to
// itself. An ambiguous name resolves to itself when no candidate is a
// partition or partitioned (plain: each is its own root) and is left out
// (unresolved) otherwise.
func (c catalogRoots) add(rel Relation, matches int64, plain bool, schema, name *string,
	leaves *int64) {
	if matches > 1 && plain {
		c[rel] = Root{Schema: rel.Schema, Name: rel.Name}
		return
	}
	if matches != 1 {
		return
	}
	root := Root{Schema: rel.Schema, Name: rel.Name}
	if schema != nil && name != nil {
		root = Root{Schema: *schema, Name: *name}
		if leaves != nil {
			root.Leaves = int(*leaves)
		}
	}
	c[rel] = root
}
