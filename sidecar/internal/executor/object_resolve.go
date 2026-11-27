package executor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// objectCandidates are the types or operators of one name in one schema:
// how many there are, and whether every one is trusted (typeCandidatesSQL,
// operatorCandidatesSQL).
type objectCandidates struct {
	count   int
	trusted bool
}

// objectLookup reads the candidates for a qualified type or operator.
type objectLookup func(ctx context.Context, ref optimizer.ObjectRef) (objectCandidates, error)

// untrustedDeps is true when the object $class/$oid depends (pg_depend) on a
// function or operator that is not an extension's member. pg_catalog's
// objects are pinned and never recorded, and an extension's member cannot
// be planted by another role.
const untrustedDeps = `EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
      WHERE d.classid = %s AND d.objid = %s
        AND d.refclassid IN ('pg_catalog.pg_proc'::pg_catalog.regclass,
                             'pg_catalog.pg_operator'::pg_catalog.regclass)
        AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend m
          WHERE m.classid = d.refclassid AND m.objid = d.refobjid
            AND m.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
            AND m.deptype = 'e'))`

// typeMember is true when type o is an extension's member.
const typeMember = `EXISTS (SELECT 1 FROM pg_catalog.pg_depend m
      WHERE m.classid = 'pg_catalog.pg_type'::pg_catalog.regclass AND m.objid = o.oid
        AND m.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
        AND m.deptype = 'e')`

// typeCandidatesSQL counts a qualified type and checks that it, and every
// domain it is built on, runs no code another role could plant: it is in
// pg_catalog or an extension's member, or it is an enum or a domain whose
// CHECK constraints call only pg_catalog and extension functions, with no
// cast function outside them. An application's own enum (state =
// 'open'::public.state, as pg_get_indexdef prints it under the pin) passes;
// a domain whose CHECK calls a planted function does not.
var typeCandidatesSQL = `/* pg_sage executor */ WITH RECURSIVE chain AS (
  SELECT o.oid, 0 AS depth FROM pg_catalog.pg_type o
  JOIN pg_catalog.pg_namespace n ON n.oid = o.typnamespace
  WHERE n.nspname = $1 AND o.typname = $2
  UNION ALL
  SELECT b.typbasetype, c.depth + 1 FROM chain c
  JOIN pg_catalog.pg_type b ON b.oid = c.oid
  WHERE b.typtype = 'd' AND c.depth < 32)
SELECT count(*) FILTER (WHERE c.depth = 0), COALESCE(bool_and(
    o.typnamespace = 'pg_catalog'::pg_catalog.regnamespace OR ` + typeMember + ` OR (
      o.typtype IN ('e', 'd')
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k
        WHERE k.contypid = o.oid AND k.contype = 'c' AND ` +
	fmt.Sprintf(untrustedDeps, "'pg_catalog.pg_constraint'::pg_catalog.regclass", "k.oid") + `)
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_cast k
        JOIN pg_catalog.pg_proc f ON f.oid = k.castfunc
        WHERE (k.castsource = o.oid OR k.casttarget = o.oid)
          AND f.pronamespace <> 'pg_catalog'::pg_catalog.regnamespace
          AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend m
            WHERE m.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND m.objid = f.oid
              AND m.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
              AND m.deptype = 'e')))), false)
FROM chain c JOIN pg_catalog.pg_type o ON o.oid = c.oid`

// operatorCandidatesSQL counts a qualified operator's definitions and
// checks that each is an extension's member or runs a pg_catalog function
// (oprcode), the code an index evaluates.
const operatorCandidatesSQL = `/* pg_sage executor */ SELECT count(*), COALESCE(bool_and(
    f.pronamespace = 'pg_catalog'::pg_catalog.regnamespace OR EXISTS (
      SELECT 1 FROM pg_catalog.pg_depend m
      WHERE m.classid = 'pg_catalog.pg_operator'::pg_catalog.regclass AND m.objid = o.oid
        AND m.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
        AND m.deptype = 'e')), false)
FROM pg_catalog.pg_operator o
JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace
LEFT JOIN pg_catalog.pg_proc f ON f.oid = o.oprcode
WHERE n.nspname = $1 AND o.oprname = $2`

// poolObjectLookup reads object candidates in a bounded read-only catalog
// transaction under the pinned search_path.
func poolObjectLookup(pool *pgxpool.Pool) objectLookup {
	return func(ctx context.Context, ref optimizer.ObjectRef) (objectCandidates, error) {
		query := typeCandidatesSQL
		if ref.Kind == optimizer.ObjectOperator {
			query = operatorCandidatesSQL
		}
		var c objectCandidates
		err := queryRowPinned(ctx, pool, query, []any{ref.Schema, ref.Name},
			&c.count, &c.trusted)
		if err != nil {
			return objectCandidates{}, err
		}
		return c, nil
	}
}

// checkIndexObjects refuses a CREATE INDEX whose expressions name a type
// (a cast or typed literal) or an operator (OPERATOR(s.op)) in a schema
// other than pg_catalog that could run code another role planted (a
// domain's CHECK, a cast function, an operator's function). Like a
// qualified call it binds that schema whatever the search_path, and the
// code would run as the table owner during the build. Other statements
// pass unchanged.
func checkIndexObjects(ctx context.Context, lookup objectLookup, sql string) error {
	if !isCreateIndexSQL(sql) {
		return nil
	}
	refs, err := optimizer.IndexObjectRefs(sql)
	if err != nil {
		return fmt.Errorf("%w: the index expressions cannot be verified: %v",
			ErrFunctionSchemaNotAllowed, err)
	}
	seen := map[optimizer.ObjectRef]bool{}
	for _, r := range refs {
		if seen[r] {
			continue
		}
		seen[r] = true
		c, err := lookup(ctx, r)
		if err != nil {
			return fmt.Errorf("resolve %s for the index: %w", r, err)
		}
		if c.count == 0 || !c.trusted {
			return fmt.Errorf("%w: %s could run code outside pg_catalog and the "+
				"extensions (another role may have created it in %s)",
				ErrFunctionSchemaNotAllowed, r, r.Schema)
		}
	}
	return nil
}

// checkStatementsObjects applies checkIndexObjects to each statement.
func checkStatementsObjects(ctx context.Context, lookup objectLookup,
	statements []string) error {
	for _, sql := range statements {
		if err := checkIndexObjects(ctx, lookup, sql); err != nil {
			return err
		}
	}
	return nil
}
