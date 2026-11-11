package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// ErrUnresolvedOpclass marks a CREATE INDEX whose bare operator class the
// executor will not resolve for its pinned DDL session: a class defined
// outside every extension, or by more than one extension.
var ErrUnresolvedOpclass = errors.New(
	"operator class does not resolve under the pinned search_path")

// opclassCandidates are the operator classes of one name and access
// method: whether pg_catalog has one, and the schemas of the classes that
// belong to an installed extension and of any others.
type opclassCandidates struct {
	catalog          bool
	extension, other []string
}

// opclassLookup reads the candidates for an operator class name and
// access method.
type opclassLookup func(ctx context.Context, name, method string) (opclassCandidates, error)

// opclassCandidatesSQL lists where an operator class of a name and access
// method lives. A class counts as an extension's when it is a member
// (pg_depend, deptype 'e') in that extension's own schema, the rule the
// optimizer's expression check applies to functions.
const opclassCandidatesSQL = `/* pg_sage executor */ SELECT
  COALESCE(bool_or(n.nspname = 'pg_catalog'), false),
  COALESCE(array_agg(n.nspname::text ORDER BY n.nspname)
    FILTER (WHERE n.nspname <> 'pg_catalog' AND x.member), '{}'::text[]),
  COALESCE(array_agg(n.nspname::text ORDER BY n.nspname)
    FILTER (WHERE n.nspname <> 'pg_catalog' AND NOT x.member), '{}'::text[])
FROM (SELECT c.opcnamespace, EXISTS (
    SELECT 1 FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_extension e ON e.oid = d.refobjid
    WHERE d.classid = 'pg_catalog.pg_opclass'::pg_catalog.regclass
      AND d.objid = c.oid
      AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
      AND d.deptype = 'e'
      AND e.extnamespace = c.opcnamespace) AS member
  FROM pg_catalog.pg_opclass c
  JOIN pg_catalog.pg_am a ON a.oid = c.opcmethod
  WHERE c.opcname = $1 AND a.amname = $2) x
JOIN pg_catalog.pg_namespace n ON n.oid = x.opcnamespace`

// poolOpclassLookup reads operator class candidates in a bounded read-only
// catalog transaction under the pinned search_path.
func poolOpclassLookup(pool *pgxpool.Pool) opclassLookup {
	return func(ctx context.Context, name, method string) (opclassCandidates, error) {
		var c opclassCandidates
		err := queryRowPinned(ctx, pool, opclassCandidatesSQL, []any{name, method},
			&c.catalog, &c.extension, &c.other)
		if err != nil {
			return opclassCandidates{}, err
		}
		return c, nil
	}
}

// qualifyIndexOpclasses names the schema of each bare operator class a
// CREATE INDEX takes from an installed extension (pg_trgm's gin_trgm_ops,
// pgvector's vector_l2_ops), so the statement still builds under the
// executor's search_path = pg_catalog, pg_temp. Only operator classes are
// resolved: creating one takes a superuser or a trusted extension, so
// another role cannot plant one, while functions, types and tables keep
// failing closed. Other statements, and a CREATE INDEX the DDL scanner
// cannot read, are returned unchanged for PostgreSQL to judge.
func qualifyIndexOpclasses(ctx context.Context, lookup opclassLookup, sql string) (string, error) {
	if !isCreateIndexSQL(sql) {
		return sql, nil
	}
	method, refs, err := optimizer.IndexOpclasses(sql)
	if err != nil {
		return sql, nil
	}
	schemas, err := opclassSchemas(ctx, lookup, method, refs)
	if err != nil {
		return "", err
	}
	out := sql
	for i := len(refs) - 1; i >= 0; i-- {
		if schemas[i] != "" {
			at := refs[i].Start
			out = out[:at] + sanitize.QuoteIdentifier(schemas[i]) + "." + out[at:]
		}
	}
	return out, nil
}

// opclassSchemas resolves each bare reference in key order ("" where none
// is to be named), reading each distinct name once.
func opclassSchemas(ctx context.Context, lookup opclassLookup, method string,
	refs []optimizer.OpclassRef) ([]string, error) {
	schemas := make([]string, len(refs))
	seen := map[string]string{}
	for i, r := range refs {
		if r.Schema != "" {
			continue
		}
		schema, ok := seen[r.Name]
		if !ok {
			c, err := lookup(ctx, r.Name, method)
			if err != nil {
				return nil, fmt.Errorf("resolve operator class %s for %s: %w", r.Name, method, err)
			}
			if schema, err = pickOpclassSchema(r.Name, method, c); err != nil {
				return nil, err
			}
			seen[r.Name] = schema
		}
		schemas[i] = schema
	}
	return schemas, nil
}

// pickOpclassSchema chooses the schema to name on a bare operator class.
// None when pg_catalog has the class: the bare name binds it under the pin
// as it did on the role's default path, which searches pg_catalog first.
// None when no schema has it: PostgreSQL reports that. The extension's
// schema when exactly one installed extension defines it. A class found
// only outside extensions, or in several extensions, is refused.
func pickOpclassSchema(name, method string, c opclassCandidates) (string, error) {
	switch {
	case c.catalog:
		return "", nil
	case len(c.extension) == 1:
		return c.extension[0], nil
	case len(c.extension) > 1:
		return "", fmt.Errorf("%w: %s for %s is defined by more than one extension (%s); "+
			"name its schema", ErrUnresolvedOpclass, name, method, strings.Join(c.extension, ", "))
	case len(c.other) > 0:
		return "", fmt.Errorf("%w: %s for %s is not part of an extension installed in its "+
			"schema (found in %s); name its schema", ErrUnresolvedOpclass, name, method,
			strings.Join(c.other, ", "))
	}
	return "", nil
}

// qualifyStatementsOpclasses applies qualifyIndexOpclasses to each
// statement of a transaction, leaving the input slice untouched.
func qualifyStatementsOpclasses(ctx context.Context, lookup opclassLookup,
	statements []string) ([]string, error) {
	out := make([]string, len(statements))
	for i, sql := range statements {
		q, err := qualifyIndexOpclasses(ctx, lookup, sql)
		if err != nil {
			return nil, err
		}
		out[i] = q
	}
	return out, nil
}

// pinnedStatements returns the statements as they run in a pinned DDL
// session, after refusing any whose index expressions would bind a
// function, type or operator another role could have planted
// (checkIndexFunctions, checkIndexObjects); a
// statement whose text the qualification changed is validated again
// before it runs.
func pinnedStatements(ctx context.Context, pool *pgxpool.Pool, statements ...string,
) ([]string, error) {
	run, err := qualifyStatementsOpclasses(ctx, poolOpclassLookup(pool), statements)
	if err != nil {
		return nil, err
	}
	if err := checkStatementsFunctions(ctx, poolFunctionLookup(pool), run); err != nil {
		return nil, err
	}
	if err := checkStatementsObjects(ctx, poolObjectLookup(pool), run); err != nil {
		return nil, err
	}
	for i := range run {
		if run[i] == statements[i] {
			continue
		}
		if err := ValidateExecutorSQL(run[i]); err != nil {
			return nil, fmt.Errorf("SQL validation after naming operator class schemas: %w", err)
		}
	}
	return run, nil
}

func isCreateIndexSQL(sql string) bool {
	upper := strings.ToUpper(normalizeSQLText(sql))
	return strings.HasPrefix(upper, "CREATE INDEX ") ||
		strings.HasPrefix(upper, "CREATE UNIQUE INDEX ")
}
