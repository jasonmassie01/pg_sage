package agentposture

import (
	"context"
	"fmt"
)

func init() { Register(ap02{}) }

// ap02 reports objects owned by agent roles (spec §6.6: agent roles own no
// objects). An owner can drop, alter and re-grant its objects whatever
// pg_sage grants. Critical for a registered agent role, a warning for a
// client-name hint.
type ap02 struct{}

func (ap02) Spec() Spec {
	return Spec{ID: "AP-02", Title: "Objects owned by agent roles", Severity: Critical}
}

// ap02Examples is how many owned objects a finding names.
const ap02Examples = 5

// ap02SQL counts each agent role's owned objects in this database and in
// the cluster's shared catalogs, from the ownership dependencies.
var ap02SQL = Statement("AP-02", `SELECT d.refobjid, count(*)::int,
  (array_agg(pg_catalog.pg_describe_object(d.classid, d.objid, d.objsubid)
     ORDER BY d.classid, d.objid))[1:$2]
FROM pg_catalog.pg_shdepend d
WHERE d.refclassid = 'pg_catalog.pg_authid'::pg_catalog.regclass AND d.deptype = 'o'
  AND d.refobjid = ANY($1::oid[])
  AND d.dbid IN (0, (SELECT oid FROM pg_catalog.pg_database
                     WHERE datname = pg_catalog.current_database()))
GROUP BY d.refobjid
ORDER BY d.refobjid`)

func (ap02) Detect(ctx context.Context, in Input) ([]Finding, error) {
	if len(in.Env.Agents) == 0 {
		return nil, nil
	}
	rows, err := in.Q.Query(ctx, ap02SQL, in.Env.AgentOIDs(), ap02Examples)
	if err != nil {
		return nil, fmt.Errorf("read objects owned by agent roles: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var oid uint32
		var n int
		var examples []string
		if err := rows.Scan(&oid, &n, &examples); err != nil {
			return nil, fmt.Errorf("read objects owned by agent roles: %w", err)
		}
		role, ok := in.Env.Agent(oid)
		if !ok {
			continue
		}
		out = append(out, ap02Finding(role, n, examples))
	}
	return out, rows.Err()
}

func ap02Finding(role Role, n int, examples []string) Finding {
	ev := make([]Evidence, len(examples))
	for i, x := range examples {
		ev[i] = Evidence{Source: "pg_shdepend", Ref: x, Detail: "owned by " + role.Name}
	}
	f := Finding{Severity: Critical, ObjectType: "role", Object: role.Name,
		Title: fmt.Sprintf("Agent role %s owns %s", role.Name, plural(n, "object")),
		Detail: fmt.Sprintf("Registered agent role %s owns %s in this database or the "+
			"cluster. An owner can drop or alter its objects and grant them to anyone, "+
			"whatever privileges pg_sage manages.", role.Name, plural(n, "object")),
		Recommendation: "Move the objects to an application owner role; agent roles " +
			"own nothing.",
		FixScript: fmt.Sprintf("-- Replace app_owner with the role that should own them.\n"+
			"REASSIGN OWNED BY %s TO app_owner;", QuoteIdent(role.Name)),
		Caveat: "REASSIGN OWNED works per database; run it in every database where " +
			"the role owns objects.",
		Evidence: ev}
	if !role.Registered() {
		f.Severity = Warning
		f.Detail = fmt.Sprintf("Role %s connects with %q, which looks like an agent "+
			"client (a hint, not a registered agent), and owns %s.", role.Name, role.Hint,
			plural(n, "object"))
	}
	return f
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
