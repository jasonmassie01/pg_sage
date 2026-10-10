package grants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
)

// parseObject splits schema.name; either part may be double-quoted.
func parseObject(s string) (string, string, error) {
	if len(s) == 0 || len(s) > 300 || strings.ContainsRune(s, 0) {
		return "", "", invalidf("object %q must be schema.name", s)
	}
	var parts []string
	rest := s
	for {
		part, tail, err := identPart(rest)
		if err != nil {
			return "", "", invalidf("object %q: %v", s, err)
		}
		parts = append(parts, part)
		if tail == "" {
			break
		}
		if tail[0] != '.' {
			return "", "", invalidf("object %q must be schema.name", s)
		}
		rest = tail[1:]
	}
	if len(parts) != 2 {
		return "", "", invalidf("object %q must be schema.name", s)
	}
	return parts[0], parts[1], nil
}

// identPart reads one identifier: "quoted" (with "" escapes) or a plain
// run of letters, digits, _ and $.
func identPart(s string) (string, string, error) {
	if strings.HasPrefix(s, `"`) {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '"' {
				b.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"')
				i++
				continue
			}
			if b.Len() == 0 || b.Len() > 63 {
				return "", "", errors.New("a quoted name must be 1-63 characters")
			}
			return b.String(), s[i+1:], nil
		}
		return "", "", errors.New("unterminated quoted name")
	}
	n := 0
	for n < len(s) && (s[n] == '_' || s[n] == '$' || s[n] >= '0' && s[n] <= '9' ||
		s[n] >= 'a' && s[n] <= 'z' || s[n] >= 'A' && s[n] <= 'Z' || s[n] >= 0x80) {
		n++
	}
	if n == 0 || n > 63 {
		return "", "", errors.New("a name must be 1-63 characters")
	}
	return s[:n], s[n:], nil
}

func ident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

func identList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = ident(c)
	}
	return strings.Join(q, ", ")
}

func grantOptionFix(schema, rel string, cols []string, me string) string {
	return fmt.Sprintf("GRANT SELECT (%s) ON TABLE %s TO %s WITH GRANT OPTION;",
		identList(cols), ident(schema, rel), ident(me))
}

func schemaOptionFix(schema, me string) string {
	return fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s WITH GRANT OPTION;", ident(schema),
		ident(me))
}

func publicCreateFix(schema string) string {
	return fmt.Sprintf("REVOKE CREATE ON SCHEMA %s FROM PUBLIC;", ident(schema))
}

// column is one live column with whether pg_sage itself holds SELECT on
// it WITH GRANT OPTION (owner-role membership does not count).
type column struct {
	col      classify.Column
	grantOpt bool
}

// relation is one resolved object.
type relation struct {
	schema, name     string
	oid, schemaOID   uint32
	me               string
	schemaGrantOpt   bool
	publicCreate     bool
	columns          []column
	grant            []string // the columns this grant lists
	excluded         []Exclusion
	classes          classify.RelationClasses
	requestedColumns []string
}

// resolveSQL reads a relation's columns and pg_sage's own grant options,
// and whether PUBLIC may CREATE in its schema (P1).
const resolveSQL = `/* pg_sage guard_grant v1 */
WITH me AS (SELECT oid, rolname::text AS name FROM pg_catalog.pg_roles
            WHERE rolname = current_user)
SELECT c.oid::int8, n.oid::int8, a.attnum, a.attname::text,
  pg_catalog.format_type(a.atttypid, a.atttypmod), me.name,
  EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(c.relacl,
            pg_catalog.acldefault('r', c.relowner))) x
          WHERE x.grantee = me.oid AND x.privilege_type = 'SELECT' AND x.is_grantable)
  OR EXISTS (SELECT 1 FROM pg_catalog.aclexplode(a.attacl) x
          WHERE x.grantee = me.oid AND x.privilege_type = 'SELECT' AND x.is_grantable),
  EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(n.nspacl,
            pg_catalog.acldefault('n', n.nspowner))) x
          WHERE x.grantee = me.oid AND x.privilege_type = 'USAGE' AND x.is_grantable),
  pg_catalog.has_schema_privilege('public', n.oid, 'CREATE')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0
  AND NOT a.attisdropped
CROSS JOIN me
WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'v', 'm', 'p', 'f')
ORDER BY a.attnum`

func resolveRelation(ctx context.Context, q Querier, schema, name string) (*relation,
	error) {
	rows, err := q.Query(ctx, resolveSQL, schema, name)
	if err != nil {
		return nil, fmt.Errorf("grants: resolving %s.%s: %w", schema, name, err)
	}
	defer rows.Close()
	r := &relation{schema: schema, name: name}
	for rows.Next() {
		var c column
		var oid, nsp int64
		if err := rows.Scan(&oid, &nsp, &c.col.AttNum, &c.col.Name, &c.col.Type, &r.me,
			&c.grantOpt, &r.schemaGrantOpt, &r.publicCreate); err != nil {
			return nil, fmt.Errorf("grants: resolving %s.%s: %w", schema, name, err)
		}
		r.oid, r.schemaOID = uint32(oid), uint32(nsp)
		c.col.RelID, c.col.Schema, c.col.Table = r.oid, schema, name
		r.columns = append(r.columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("grants: resolving %s.%s: %w", schema, name, err)
	}
	if len(r.columns) == 0 {
		return nil, fmt.Errorf("%w: relation %s does not exist", agentguard.ErrNotFound,
			ident(schema, name))
	}
	return r, nil
}

func (r *relation) qualified() string { return r.schema + "." + r.name }

// planRelations resolves every object and decides its column list.
func (m *Manager) planRelations(ctx context.Context, req GrantRequest) ([]*relation, error) {
	classes := classify.NewStore(req.Target.Pool)
	var out []*relation
	for _, o := range req.Objects {
		schema, name, _ := parseObject(o.Object)
		r, err := resolveRelation(ctx, req.Target.Pool, schema, name)
		if err != nil {
			return nil, err
		}
		if r.publicCreate {
			return nil, deny(agentguard.ReasonPublicCreate, publicCreateFix(schema),
				"PUBLIC can CREATE in schema %s, so every agent role could; preflight P1 "+
					"refuses grants there", schema)
		}
		if r.classes, err = classes.Lookup(ctx, r.oid); err != nil {
			return nil, fmt.Errorf("grants: classification of %s: %w", r.qualified(), err)
		}
		r.requestedColumns = o.Columns
		if err := m.chooseColumns(r, req); err != nil {
			return nil, err
		}
		if !r.schemaGrantOpt {
			return nil, deny(agentguard.ReasonGrantorLacksPrivilege,
				schemaOptionFix(schema, r.me), "pg_sage's role %s lacks USAGE WITH GRANT "+
					"OPTION on schema %s", r.me, schema)
		}
		out = append(out, r)
	}
	return out, nil
}

// chooseColumns applies classification (classify.Grantable in the
// evaluated environment) and pg_sage's grant options. Explicit columns
// must all pass; with none named, the passing columns are listed and the
// rest reported as excluded.
func (m *Manager) chooseColumns(r *relation, req GrantRequest) error {
	byName := map[string]column{}
	var cols []classify.Column
	for _, c := range r.columns {
		byName[c.col.Name] = c
		cols = append(cols, c.col)
	}
	if len(r.requestedColumns) > 0 {
		cols = cols[:0]
		for _, name := range r.requestedColumns {
			c, ok := byName[name]
			if !ok {
				return fmt.Errorf("%w: column %s of %s", agentguard.ErrNotFound, name,
					r.qualified())
			}
			cols = append(cols, c.col)
		}
	}
	unmasked := func(c classify.Column) bool {
		return m.cfg.Unmasked != nil && m.cfg.Unmasked(req.PrincipalID, c)
	}
	allowed, excluded := classify.Grantable(effectiveEnv(req.Target.Env), cols, r.classes,
		unmasked)
	if len(r.requestedColumns) > 0 && len(excluded) > 0 {
		x := excluded[0]
		return deny(decide.ReasonClassification, "", "column %s of %s is %s in %s",
			x.Column.Name, r.qualified(), x.Reason, effectiveEnv(req.Target.Env))
	}
	for _, x := range excluded {
		r.excluded = append(r.excluded, Exclusion{Object: r.qualified(),
			Column: x.Column.Name, Reason: x.Reason})
	}
	return r.applyGrantOptions(allowed, len(r.requestedColumns) > 0, byName)
}

func (r *relation) applyGrantOptions(allowed []classify.Column, explicit bool,
	byName map[string]column) error {
	var lacking []string
	for _, c := range allowed {
		if byName[c.Name].grantOpt {
			r.grant = append(r.grant, c.Name)
			continue
		}
		lacking = append(lacking, c.Name)
		r.excluded = append(r.excluded, Exclusion{Object: r.qualified(), Column: c.Name,
			Reason: string(agentguard.ReasonGrantorLacksPrivilege)})
	}
	if explicit && len(lacking) > 0 {
		return deny(agentguard.ReasonGrantorLacksPrivilege,
			grantOptionFix(r.schema, r.name, lacking, r.me), "pg_sage's role %s lacks "+
				"SELECT WITH GRANT OPTION on %s of %s", r.me, strings.Join(lacking, ", "),
			r.qualified())
	}
	if len(r.grant) == 0 {
		return deny(decide.ReasonClassification, "", "no column of %s may be granted "+
			"in this environment", r.qualified())
	}
	return nil
}
