package agentposture

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
)

// SourceGroup marks a role an exposed role inherits privileges from.
const SourceGroup RoleSource = "group"

// Path is how an exposed role inherits a group role's privileges: Names
// runs from the exposed role to the group, both included, one hop per
// inheriting membership.
type Path struct {
	Exposed uint32
	Names   []string
}

// String is the path as "anon → g1 → g2".
func (p Path) String() string { return strings.Join(p.Names, " → ") }

// maxViaDepth bounds the inheriting memberships one path may cross.
const maxViaDepth = 16

// maxMemberships bounds the membership read; a variable so tests can reach
// the limit with a few rows.
var maxMemberships = 10000

// membershipSQL reads the inheriting memberships reachable from the
// exposed roles ($1): member, its name, the group and its name. UNION
// keeps each membership once, so the recursion ends on any graph. %[1]s
// is the version's inheritance test on membership m of member role r.
const membershipSQL = `WITH RECURSIVE e(member, roleid) AS (
  SELECT m.member, m.roleid FROM pg_catalog.pg_auth_members m
  JOIN pg_catalog.pg_roles r ON r.oid = m.member
  WHERE m.member = ANY($1::oid[]) AND %[1]s
  UNION
  SELECT m.member, m.roleid FROM e
  JOIN pg_catalog.pg_auth_members m ON m.member = e.roleid
  JOIN pg_catalog.pg_roles r ON r.oid = m.member
  WHERE %[1]s)
SELECT e.member, r.rolname::text, e.roleid, g.rolname::text
FROM e
JOIN pg_catalog.pg_roles r ON r.oid = e.member
JOIN pg_catalog.pg_roles g ON g.oid = e.roleid
LIMIT $2`

// Before PostgreSQL 16 a member inherits when its role has rolinherit;
// from 16 each membership carries its own INHERIT option.
var (
	membershipRoleLevelSQL = Statement("env", fmt.Sprintf(membershipSQL, "r.rolinherit"))
	membershipPerGrantSQL  = Statement("env", fmt.Sprintf(membershipSQL, "m.inherit_option"))
)

// membership is one inheriting membership: member inherits group.
type membership struct {
	member, group         uint32
	memberName, groupName string
}

// resolveVia fills env.Via with the groups the exposed roles inherit.
// Without an exposed role other than PUBLIC there is nothing to read.
func resolveVia(ctx context.Context, q Querier, env *Env) error {
	var members []uint32
	for _, r := range env.Exposed {
		if r.OID != PublicOID {
			members = append(members, r.OID)
		}
	}
	if len(members) == 0 {
		return nil
	}
	stmt := membershipRoleLevelSQL
	if env.VersionNum >= 160000 {
		stmt = membershipPerGrantSQL
	}
	rows, err := q.Query(ctx, stmt, members, maxMemberships+1)
	if err != nil {
		return fmt.Errorf("agent posture: read role memberships of exposed roles: %w", err)
	}
	defer rows.Close()
	var edges []membership
	for rows.Next() {
		if len(edges) == maxMemberships {
			env.ViaTruncated = append(env.ViaTruncated,
				Truncation{Arm: "memberships", Limit: maxMemberships})
			break
		}
		var m membership
		if err := rows.Scan(&m.member, &m.memberName, &m.group, &m.groupName); err != nil {
			return fmt.Errorf("agent posture: read role memberships of exposed roles: %w", err)
		}
		edges = append(edges, m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("agent posture: read role memberships of exposed roles: %w", err)
	}
	via, cut := buildVia(env.Exposed, edges, maxViaDepth)
	env.Via = via
	if cut {
		env.ViaTruncated = append(env.ViaTruncated,
			Truncation{Arm: "membership levels", Limit: maxViaDepth})
	}
	return nil
}

// buildVia maps each group to the shortest inheritance path of every
// exposed role that inherits it, at most depth memberships long. A path
// never returns to a role it crossed, so cycles end. cut reports a group
// left out because it lies deeper than depth.
func buildVia(exposed []Role, edges []membership, depth int) (map[uint32][]Path, bool) {
	next := map[uint32][]membership{}
	for _, m := range edges {
		next[m.member] = append(next[m.member], m)
	}
	for k := range next {
		slices.SortFunc(next[k], func(a, b membership) int {
			return cmp.Or(cmp.Compare(a.groupName, b.groupName), cmp.Compare(a.group, b.group))
		})
	}
	via := map[uint32][]Path{}
	cut := false
	for _, e := range exposed {
		if e.OID == PublicOID {
			continue
		}
		if walkVia(e, next, depth, via) {
			cut = true
		}
	}
	return via, cut
}

// walkVia adds e's shortest path to each group it reaches, breadth first;
// it reports whether a group lay deeper than depth.
func walkVia(e Role, next map[uint32][]membership, depth int,
	via map[uint32][]Path) bool {
	type step struct {
		oid   uint32
		names []string
	}
	seen := map[uint32]bool{e.OID: true}
	level := []step{{oid: e.OID, names: []string{e.Name}}}
	for hops := 1; len(level) > 0; hops++ {
		var deeper []step
		for _, s := range level {
			for _, m := range next[s.oid] {
				if seen[m.group] {
					continue
				}
				if hops > depth {
					return true
				}
				seen[m.group] = true
				names := append(slices.Clone(s.names), m.groupName)
				via[m.group] = append(via[m.group], Path{Exposed: e.OID, Names: names})
				deeper = append(deeper, step{oid: m.group, names: names})
			}
		}
		level = deeper
	}
	return false
}
