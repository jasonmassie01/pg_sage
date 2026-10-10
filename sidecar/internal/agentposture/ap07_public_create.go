package agentposture

import (
	"context"
	"fmt"
)

func init() { Register(ap07{}) }

// ap07 reports schemas where PUBLIC may create objects: any role, agents
// included, can plant a function or table there that shadows the one a
// definer function or another user's query expects (PostgreSQL 14's public
// schema is the common case).
type ap07 struct{}

func (ap07) Spec() Spec {
	return Spec{ID: "AP-07", Title: "Schemas where PUBLIC can create", Severity: Warning}
}

var ap07SQL = Statement("AP-07", `SELECT n.nspname::text
FROM pg_catalog.pg_namespace n
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'
  AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(
        COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
      WHERE a.grantee = 0 AND a.privilege_type = 'CREATE')
ORDER BY n.nspname
LIMIT $1`)

func (ap07) Detect(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap07SQL, maxRows)
	if err != nil {
		return nil, fmt.Errorf("read schema ACLs: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return nil, fmt.Errorf("read schema ACLs: %w", err)
		}
		out = append(out, Finding{Severity: Warning, ObjectType: "schema", Object: schema,
			Title: fmt.Sprintf("Any role can create objects in schema %s", schema),
			Detail: fmt.Sprintf("PUBLIC holds CREATE on schema %s, so every role, agents "+
				"included, can add tables and functions there, including objects that "+
				"shadow the ones other code finds through search_path.", schema),
			Recommendation: "Revoke CREATE from PUBLIC and grant it only to the roles " +
				"that own objects in the schema.",
			FixScript: fmt.Sprintf("REVOKE CREATE ON SCHEMA %s FROM PUBLIC;",
				QuoteIdent(schema)),
			Caveat: "Grant CREATE to the roles that deploy into the schema first, or " +
				"their migrations fail.",
			Evidence: []Evidence{{Source: "pg_namespace.nspacl", Ref: schema,
				Detail: "PUBLIC: CREATE"}}})
	}
	return out, rows.Err()
}
