package rolegrants

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// SequenceNeed is what reading sequences is for.
const SequenceNeed = "the first look's sequence runway (each sequence's last value)"

// SequenceSelect is whether the connected role can read the user
// sequences, per schema. Names are quoted as SQL needs them.
type SequenceSelect struct {
	Schemas []string `json:"schemas"`
	Missing []string `json:"missing"`
}

// maxSequenceSchemas bounds the schemas read; a GRANT naming more would not
// fit a log line or a guide row anyway.
const maxSequenceSchemas = 200

// sequenceSelectSQL lists the schemas holding sequences, outside the
// system schemas and pg_sage's sage schema (the first look's user
// schemas), and whether the role can read every sequence there. Readable
// is the first look's test: SELECT or USAGE, either of which lets
// pg_sequence_last_value read it. has_sequence_privilege is NULL for a
// sequence dropped while the probe runs; it needs no grant.
const sequenceSelectSQL = `/* pg_sage grants */ SELECT n.nspname::text,
  COALESCE(bool_and(pg_catalog.has_sequence_privilege(c.oid, 'SELECT,USAGE')), true)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'
GROUP BY n.nspname
ORDER BY n.nspname
LIMIT $1`

// CheckSequenceSelect reads, live, which schemas hold sequences the
// connected role cannot read.
func CheckSequenceSelect(ctx context.Context, pool *pgxpool.Pool) (SequenceSelect, error) {
	if pool == nil {
		return SequenceSelect{}, ErrNoPool
	}
	return readSequenceSelect(ctx, pool)
}

// querier is what the probe reads through: the pool, or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readSequenceSelect(ctx context.Context, q querier) (SequenceSelect, error) {
	rows, err := q.Query(ctx, sequenceSelectSQL, maxSequenceSchemas)
	if err != nil {
		return SequenceSelect{}, fmt.Errorf("read sequence SELECT privileges: %w", err)
	}
	defer rows.Close()
	var out SequenceSelect
	for rows.Next() {
		var name string
		var ok bool
		if err := rows.Scan(&name, &ok); err != nil {
			return SequenceSelect{}, fmt.Errorf("read sequence SELECT privileges: %w", err)
		}
		quoted := sqlident.Ident(name)
		out.Schemas = append(out.Schemas, quoted)
		if !ok {
			out.Missing = append(out.Missing, quoted)
		}
	}
	if err := rows.Err(); err != nil {
		return SequenceSelect{}, fmt.Errorf("read sequence SELECT privileges: %w", err)
	}
	return out, nil
}

// Lacking reports whether a schema holds a sequence the role cannot read.
func (s SequenceSelect) Lacking() bool { return len(s.Missing) > 0 }

// What names the privilege, e.g. "SELECT on the sequences in schema app".
func (s SequenceSelect) What() string {
	return "SELECT on the sequences in " + schemaList(schemaTarget(s.Missing, s.Schemas))
}

// GrantSQL is the statement that grants it to role (already quoted).
func (s SequenceSelect) GrantSQL(role string) string {
	return SequenceSelectSQL(schemaTarget(s.Missing, s.Schemas), role)
}

// SequenceSelectSQL grants role (already quoted) SELECT on every sequence
// in the schemas (already quoted; public when none). The first look's
// degraded sequence check prints it too.
func SequenceSelectSQL(schemas []string, role string) string {
	if len(schemas) == 0 {
		schemas = []string{"public"}
	}
	return "GRANT SELECT ON ALL SEQUENCES IN SCHEMA " + strings.Join(schemas, ", ") +
		" TO " + role
}

// schemaTarget is the schemas a grant names: the missing ones, else every
// schema checked, else public, where new objects go by default.
func schemaTarget(missing, all []string) []string {
	switch {
	case len(missing) > 0:
		return missing
	case len(all) > 0:
		return all
	}
	return []string{"public"}
}

// schemaList is "schema a" or "schemas a, b".
func schemaList(t []string) string {
	if len(t) == 1 {
		return "schema " + t[0]
	}
	return "schemas " + strings.Join(t, ", ")
}
