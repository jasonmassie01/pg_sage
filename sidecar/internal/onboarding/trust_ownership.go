package onboarding

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// actingGrants are the grants of the levels that run actions, least
// privilege first: pg_maintain (PostgreSQL 17+) covers maintenance, table
// owner membership what only an owner may do.
func actingGrants(g GrantStatus, role string) []Grant {
	var out []Grant
	if g.Maintain != nil {
		out = append(out, Grant{Name: GrantMaintain,
			Why: "VACUUM, ANALYZE, REINDEX and CLUSTER any table without owning it " +
				"(PostgreSQL 17+). The least privilege for maintenance: grant it first.",
			SQL: "GRANT pg_maintain TO " + role + ";", Present: g.Maintain})
	}
	out = append(out, ownershipGrants(g, role)...)
	return append(out, schemaCreateGrant(g.SchemaCreate, role), signalGrant(g, role))
}

const ownerWhy = "CREATE and DROP INDEX and ALTER TABLE storage settings need the " +
	"privileges of the table owner. Membership in the owner role lets pg_sage do " +
	"anything the owner can, DROP and TRUNCATE included: only its typed actions and the " +
	"policy gate stop that. If your provider refuses the grant (RDS, Cloud SQL, when " +
	"the owner is held by its admin role), reassign the tables to an ordinary owner role " +
	"and grant that role instead."

const superOwnerWhy = "This owner is a superuser: membership would make pg_sage a " +
	"superuser too, able to DROP and TRUNCATE anything, past every check but its typed " +
	"actions and the policy gate. Never grant it. Reassign its tables to an ordinary " +
	"owner role (ALTER TABLE ... OWNER TO, or REASSIGN OWNED BY) and grant that role."

// ownershipGrants is one grant per owner role whose tables pg_sage's role
// cannot act on, or, when none is listed, one summary grant.
func ownershipGrants(g GrantStatus, role string) []Grant {
	if len(g.Owners) == 0 {
		return []Grant{ownershipSummary(g)}
	}
	out := make([]Grant, 0, len(g.Owners))
	for _, o := range g.Owners {
		owner := quoteRole(o.Role)
		missing := false // listed owners are those whose privileges the role lacks
		gr := Grant{Name: GrantTableOwnership + ":" + o.Role, Why: ownerWhy,
			SQL:     "GRANT " + owner + " TO " + role + ";",
			Present: &missing,
			Detail: fmt.Sprintf("%s owns %d %s", owner, o.Tables,
				plural(o.Tables, "table", "tables"))}
		if o.Superuser {
			gr.Why = superOwnerWhy
			gr.SQL = "-- superuser owner: reassign its tables to an ordinary role, " +
				"then grant that role"
			gr.Detail = "superuser " + gr.Detail
		}
		out = append(out, gr)
	}
	if rest := g.OwnerRoles - len(g.Owners); rest > 0 {
		out[len(out)-1].Detail += fmt.Sprintf("; %d more owner %s not listed", rest,
			plural(rest, "role", "roles"))
	}
	return out
}

// ownershipSummary stands for table ownership when no owner role is
// listed: unknown with no user tables, present when the role has every
// owner's privileges, missing when the owners were not read.
func ownershipSummary(g GrantStatus) Grant {
	out := Grant{Name: GrantTableOwnership, Why: ownerWhy,
		SQL: "-- no user tables yet: the owner roles to grant are listed once tables exist"}
	if g.TablesTotal == 0 {
		return out
	}
	all := g.TablesOwned == g.TablesTotal
	out.Present = &all
	out.Detail = fmt.Sprintf("owns %d of %d tables", g.TablesOwned, g.TablesTotal)
	out.SQL = "-- pg_sage's role has the privileges of every table owner"
	if !all {
		out.SQL = "-- list the owner roles to grant: SELECT DISTINCT " +
			"pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class " +
			"WHERE relkind IN ('r', 'p') AND NOT pg_catalog.pg_has_role(relowner, 'USAGE')"
	}
	return out
}

// signalGrant uses the shared probe's SQL, which fixes a membership that
// does not inherit pg_signal_backend's privileges, too.
func signalGrant(g GrantStatus, role string) Grant {
	var sig rolegrants.SignalBackend
	if g.Signal != nil {
		sig = *g.Signal
	}
	out := Grant{Name: GrantSignalBackend,
		Why: "Cancel a runaway query when an approved action needs it.",
		SQL: sig.GrantSQL(role) + ";", Present: g.SignalBackend}
	if sig.MemberOnly {
		out.Detail = "a member of pg_signal_backend that does not inherit its privileges, " +
			"so it cannot cancel queries"
	}
	return out
}

// sequenceGrant is SELECT on the sequences of the first look's user
// schemas, with the same SQL the startup check and the first look print.
func sequenceGrant(sq *rolegrants.SequenceSelect, role string) Grant {
	out := Grant{Name: GrantSequenceSelect,
		Why: "Read each sequence's current value for the first look's sequence runway.",
		SQL: rolegrants.SequenceSelectSQL(nil, role) + ";"}
	if sq == nil {
		return out
	}
	out.SQL = sq.GrantSQL(role) + ";"
	ok := !sq.Lacking()
	out.Present = &ok
	if !ok {
		out.Detail = fmt.Sprintf("sequences not readable in %d of %d schemas: %s",
			len(sq.Missing), len(sq.Schemas), nameSome(sq.Missing))
	}
	return out
}
