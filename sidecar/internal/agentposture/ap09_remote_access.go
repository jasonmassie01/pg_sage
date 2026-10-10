package agentposture

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

func init() { Register(ap09{}) }

// ap09 reports what lets an exposed role (PUBLIC first) reach outside the
// database or run code as the server: dblink functions it can execute,
// postgres_fdw/dblink_fdw wrappers, servers and user mappings it can use,
// and untrusted procedural languages that were marked trusted. All
// critical: an agent that reaches them can open connections with stored
// credentials or run code on the host.
type ap09 struct{}

func (ap09) Spec() Spec {
	return Spec{ID: "AP-09", Title: "Remote access and untrusted languages usable by exposed roles",
		Severity: Critical}
}

// remoteWrappers are the foreign-data wrappers that open connections.
var remoteWrappers = []string{"postgres_fdw", "dblink_fdw"}

const aclGrantees = `ARRAY(SELECT u.grantee FROM pg_catalog.aclexplode(%s) u
    WHERE u.privilege_type = '%s')::oid[]`

func grantees(acl, privilege string) string { return fmt.Sprintf(aclGrantees, acl, privilege) }

var ap09DblinkSQL = Statement("AP-09", `SELECT pg_catalog.format('%I.%I(%s)', n.nspname,
  p.proname, pg_catalog.pg_get_function_identity_arguments(p.oid)),
  `+grantees("COALESCE(p.proacl, pg_catalog.acldefault('f', p.proowner))", "EXECUTE")+`,
  `+grantees("COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))", "USAGE")+`
FROM pg_catalog.pg_extension e
JOIN pg_catalog.pg_depend d ON d.refclassid = 'pg_catalog.pg_extension'::regclass
  AND d.refobjid = e.oid AND d.deptype = 'e' AND d.classid = 'pg_catalog.pg_proc'::regclass
JOIN pg_catalog.pg_proc p ON p.oid = d.objid
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE e.extname = 'dblink'
ORDER BY 1
LIMIT $1`)

func (ap09) Detect(ctx context.Context, in Input) ([]Finding, error) {
	out, err := ap09Dblink(ctx, in)
	if err != nil {
		return nil, err
	}
	fdw, err := ap09Foreign(ctx, in)
	if err != nil {
		return nil, err
	}
	langs, err := ap09Languages(ctx, in)
	if err != nil {
		return nil, err
	}
	return append(append(out, fdw...), langs...), nil
}

// ap09Dblink is one finding for the dblink functions exposed roles can run.
func ap09Dblink(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap09DblinkSQL, apbMaxRows)
	if err != nil {
		return nil, fmt.Errorf("read dblink function privileges: %w", err)
	}
	defer rows.Close()
	var fix []string
	var ev []Evidence
	var reached, granted []Role
	for rows.Next() {
		var fn string
		var exec, usage []uint32
		if err := rows.Scan(&fn, &exec, &usage); err != nil {
			return nil, fmt.Errorf("read dblink function privileges: %w", err)
		}
		who := reaching(exposedHolders(in.Env, exec), usage)
		if len(who) == 0 {
			continue
		}
		targets := revokeTargets(who, exec)
		for _, r := range targets {
			fix = append(fix, "REVOKE EXECUTE ON FUNCTION "+fn+" FROM "+revokeTarget(r)+";")
		}
		granted = mergeRoles(granted, targets)
		reached = mergeRoles(reached, who)
		ev = append(ev, Evidence{Source: "pg_proc.proacl", Ref: fn,
			Detail: "executable by " + roleText(who)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read dblink function privileges: %w", err)
	}
	if len(ev) == 0 {
		return nil, nil
	}
	return []Finding{{Severity: Critical, ObjectType: "extension", Object: "dblink",
		Title: "dblink is callable by exposed roles",
		Detail: fmt.Sprintf("%s can execute %d dblink functions (granted to %s): an "+
			"agent can open connections to other databases and servers from inside this "+
			"one.", roleText(reached), len(ev), roleText(granted)),
		Recommendation: "Revoke EXECUTE on the dblink functions from exposed roles; " +
			"grant them only to the roles that need remote queries.",
		FixScript: strings.Join(fix, "\n"), Evidence: ev}}, nil
}

// mergeRoles adds the roles of add not yet in rs.
func mergeRoles(rs, add []Role) []Role {
	for _, r := range add {
		if !slices.ContainsFunc(rs, func(x Role) bool { return x.OID == r.OID }) {
			rs = append(rs, r)
		}
	}
	return rs
}
