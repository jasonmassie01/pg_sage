package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// ErrFunctionSchemaNotAllowed marks a CREATE INDEX whose key expressions or
// predicate call a function the pinned DDL session must not bind: a bare
// name pg_catalog lacks, or a call that names a schema holding a function
// of that name outside every extension (security-01).
var ErrFunctionSchemaNotAllowed = errors.New("function_schema_not_allowed")

// functionCandidates are where functions of one name live: whether
// pg_catalog has one, how many the call's schema has, and whether every one
// of those is a member of an installed extension in that schema.
type functionCandidates struct {
	catalog  bool
	inSchema int
	members  bool
}

// functionLookup reads the candidates for a call (schema "" when bare).
type functionLookup func(ctx context.Context, schema, name string) (functionCandidates, error)

// functionCandidatesSQL counts a function name's definitions. A member is
// one pg_depend ties (deptype 'e') to an extension installed in the
// function's own schema; creating one takes a superuser or a trusted
// extension, so another role cannot plant one.
const functionCandidatesSQL = `/* pg_sage executor */ SELECT
  COALESCE(bool_or(n.nspname = 'pg_catalog'), false),
  count(*) FILTER (WHERE n.nspname = $2),
  COALESCE(bool_and(EXISTS (
    SELECT 1 FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_extension e ON e.oid = d.refobjid
    WHERE d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass
      AND d.objid = p.oid
      AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
      AND d.deptype = 'e'
      AND e.extnamespace = p.pronamespace)) FILTER (WHERE n.nspname = $2), false)
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE p.proname = $1`

// poolFunctionLookup reads function candidates in a bounded read-only
// catalog transaction under the pinned search_path.
func poolFunctionLookup(pool *pgxpool.Pool) functionLookup {
	return func(ctx context.Context, schema, name string) (functionCandidates, error) {
		var c functionCandidates
		err := queryRowPinned(ctx, pool, functionCandidatesSQL, []any{name, schema},
			&c.catalog, &c.inSchema, &c.members)
		if err != nil {
			return functionCandidates{}, err
		}
		return c, nil
	}
}

// checkIndexFunctions refuses a CREATE INDEX that would bind a function
// another role could have planted. Under the pinned search_path a bare
// call resolves only in pg_catalog (PostgreSQL never searches pg_temp for
// functions), so a bare name must exist there; a pg_catalog-qualified call
// and a grammar form (EXTRACT, SUBSTRING ... FROM) need no lookup; a call
// that names any other schema binds that schema, so every function of the
// name there must be an extension's member. The check runs before the
// build, because a planted function would already have run as the table
// owner during it. Other statements pass unchanged.
func checkIndexFunctions(ctx context.Context, lookup functionLookup, sql string) error {
	if !isCreateIndexSQL(sql) {
		return nil
	}
	refs, err := optimizer.IndexFunctionCalls(sql)
	if err != nil {
		return fmt.Errorf("%w: the index expressions cannot be verified: %v",
			ErrFunctionSchemaNotAllowed, err)
	}
	seen := map[optimizer.FuncRef]bool{}
	for _, r := range refs {
		key := optimizer.FuncRef{Schema: r.Schema, Name: r.Name}
		if r.Schema == "pg_catalog" || r.Grammar() || seen[key] {
			continue
		}
		seen[key] = true
		c, err := lookup(ctx, r.Schema, r.Name)
		if err != nil {
			return fmt.Errorf("resolve function %s for the index: %w", r, err)
		}
		if err := judgeFunction(r, c); err != nil {
			return err
		}
	}
	return nil
}

func judgeFunction(r optimizer.FuncRef, c functionCandidates) error {
	switch {
	case r.Schema == "" && !c.catalog:
		return fmt.Errorf("%w: %s is not a pg_catalog function, the only schema a bare "+
			"name resolves in under the pinned search_path; name its schema",
			ErrFunctionSchemaNotAllowed, r)
	case r.Schema != "" && (c.inSchema == 0 || !c.members):
		return fmt.Errorf("%w: %s is not a member of an extension installed in %s "+
			"(another role may have created it there)", ErrFunctionSchemaNotAllowed, r, r.Schema)
	}
	return nil
}

// checkStatementsFunctions applies checkIndexFunctions to each statement.
func checkStatementsFunctions(ctx context.Context, lookup functionLookup,
	statements []string) error {
	for _, sql := range statements {
		if err := checkIndexFunctions(ctx, lookup, sql); err != nil {
			return err
		}
	}
	return nil
}

// checkIndexCallSchemas is the static half (no database): a CREATE INDEX
// call may name pg_catalog but no other protected schema (sage,
// google_ml, ...), and neither may a type or operator it names. An expression the scanner cannot read is left to
// checkIndexFunctions, which refuses it before the build.
func checkIndexCallSchemas(sql, prefix string) error {
	if prefix != "CREATE INDEX" && prefix != "CREATE UNIQUE INDEX" {
		return nil
	}
	refs, err := optimizer.IndexFunctionCalls(sql)
	if err != nil {
		return nil
	}
	for _, r := range refs {
		if r.Schema != "pg_catalog" && isProtectedExecutorSchema(r.Schema) {
			return fmt.Errorf("%w: function %s is in protected schema %q",
				ErrDisallowedSQL, r, r.Schema)
		}
	}
	objects, err := optimizer.IndexObjectRefs(sql)
	if err != nil {
		return nil
	}
	for _, r := range objects {
		if isProtectedExecutorSchema(r.Schema) {
			return fmt.Errorf("%w: %s is in protected schema %q", ErrDisallowedSQL, r, r.Schema)
		}
	}
	return nil
}
