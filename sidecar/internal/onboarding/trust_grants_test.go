package onboarding

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// No concurrent access tests: TrustGuide is a pure function of its input.

// ownerLines are the per-owner-role ownership grants of a level.
func ownerLines(l Level) []Grant {
	var out []Grant
	for _, g := range l.Grants {
		if strings.HasPrefix(g.Name, GrantTableOwnership+":") {
			out = append(out, g)
		}
	}
	return out
}

func ownershipAny(l Level) []Grant {
	out := ownerLines(l)
	if g := grant(l, GrantTableOwnership); g.Name != "" {
		out = append(out, g)
	}
	return out
}

// Observation reads sequences for the runway check: the grant names
// exactly the schemas whose sequences the role cannot read.
func TestObservationSequenceGrantNamesUnreadableSchemas(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.Sequences = &rolegrants.SequenceSelect{Schemas: []string{"a", "b", "c"},
		Missing: []string{"a", "b"}}
	g := grant(level(TrustGuide(in), LevelObservation), GrantSequenceSelect)
	want := "GRANT SELECT ON ALL SEQUENCES IN SCHEMA a, b TO sage_agent;"
	if g.Present == nil || *g.Present || g.SQL != want {
		t.Fatalf("sequence grant = %+v, want missing with %q", g, want)
	}
	if !strings.HasSuffix(g.Detail, ": a, b") {
		t.Fatalf("detail %q, want exactly a and b named", g.Detail)
	}
	if !strings.Contains(strings.ToLower(g.Why), "runway") {
		t.Fatalf("why %q, want the sequence runway named", g.Why)
	}
}

func TestObservationSequenceGrantPresentOrUnknown(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.Sequences = &rolegrants.SequenceSelect{Schemas: []string{"a"}}
	if g := grant(level(TrustGuide(in), LevelObservation), GrantSequenceSelect); g.Present ==
		nil || !*g.Present {
		t.Fatalf("all readable: grant = %+v, want present", g)
	}
	// No sequences at all: nothing to read, so nothing is missing.
	in.Grants.Sequences = &rolegrants.SequenceSelect{}
	if g := grant(level(TrustGuide(in), LevelObservation), GrantSequenceSelect); g.Present ==
		nil || !*g.Present {
		t.Fatalf("no sequences: grant = %+v, want present", g)
	}
	in.Grants.Sequences = nil
	if g := grant(level(TrustGuide(in), LevelObservation), GrantSequenceSelect); g.Name == "" ||
		g.Present != nil {
		t.Fatalf("not checked: grant = %+v, want listed with Present nil", g)
	}
}

// State transitions: pg_maintain (PostgreSQL 17+) is the least privilege
// for maintenance and leads both acting levels; before 17 it does not
// exist and table ownership leads.
func TestMaintainLeadsOnSeventeen(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.Maintain = bp(false)
	in.Grants.Owners = []TableOwner{{Role: "app_owner", Tables: 7}}
	for _, name := range []string{LevelAdvisory, LevelAutonomous} {
		l := level(TrustGuide(in), name)
		first := l.Grants[0]
		if first.Name != GrantMaintain || first.SQL != "GRANT pg_maintain TO sage_agent;" {
			t.Fatalf("%s first grant = %+v, want pg_maintain", name, first)
		}
		for _, w := range []string{"VACUUM", "ANALYZE", "REINDEX", "CLUSTER"} {
			if !strings.Contains(first.Why, w) {
				t.Fatalf("%s pg_maintain why %q does not name %s", name, first.Why, w)
			}
		}
	}
	in.Grants.Maintain = nil
	for _, name := range []string{LevelAdvisory, LevelAutonomous} {
		l := level(TrustGuide(in), name)
		if !strings.HasPrefix(l.Grants[0].Name, GrantTableOwnership) {
			t.Fatalf("%s before 17: first grant = %+v, want table ownership", name,
				l.Grants[0])
		}
		if g := grant(l, GrantMaintain); g.Name != "" {
			t.Fatalf("%s before 17 lists pg_maintain: %+v", name, g)
		}
	}
}

// Ownership names each owner role and its table count, never a
// placeholder, and says plainly what membership confers.
func TestOwnershipGrantPerOwnerRole(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.TablesOwned, in.Grants.TablesTotal = 0, 415
	in.Grants.Owners = []TableOwner{{Role: "app_owner", Tables: 412},
		{Role: "batch", Tables: 3}}
	in.Grants.OwnerRoles = 2
	lines := ownerLines(level(TrustGuide(in), LevelAdvisory))
	if len(lines) != 2 {
		t.Fatalf("owner lines = %+v, want two", lines)
	}
	want := []struct{ role, count string }{{"app_owner", "412"}, {"batch", "3"}}
	for i, l := range lines {
		if l.SQL != "GRANT "+want[i].role+" TO sage_agent;" {
			t.Fatalf("line %d SQL = %q, want the grant of %s", i, l.SQL, want[i].role)
		}
		if !strings.Contains(l.Detail, want[i].role) || !strings.Contains(l.Detail,
			want[i].count) {
			t.Fatalf("line %d detail %q, want %s and %s", i, l.Detail, want[i].role,
				want[i].count)
		}
		if !strings.Contains(l.Why, "DROP") || !strings.Contains(l.Why, "TRUNCATE") ||
			!strings.Contains(l.Why, "policy gate") {
			t.Fatalf("line %d why %q, want DROP, TRUNCATE and the policy gate", i, l.Why)
		}
		if l.Present == nil || *l.Present {
			t.Fatalf("line %d present = %v, want missing", i, l.Present)
		}
	}
	if lines[0].Name == lines[1].Name {
		t.Fatalf("owner lines share the name %q", lines[0].Name)
	}
	auto := ownerLines(level(TrustGuide(in), LevelAutonomous))
	if len(auto) != 2 {
		t.Fatalf("autonomous owner lines = %+v, want the same two", auto)
	}
}

// A superuser owner is never granted: that would make pg_sage a superuser.
func TestOwnershipSuperuserOwnerIsReassigned(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.Owners = []TableOwner{{Role: "postgres", Tables: 9, Superuser: true}}
	in.Grants.OwnerRoles = 1
	lines := ownerLines(level(TrustGuide(in), LevelAdvisory))
	if len(lines) != 1 {
		t.Fatalf("owner lines = %+v, want one", lines)
	}
	l := lines[0]
	if strings.Contains(l.SQL, "GRANT postgres") || !strings.HasPrefix(l.SQL, "--") ||
		!strings.Contains(strings.ToLower(l.Why), "reassign") ||
		!strings.Contains(l.Why, "superuser") || !strings.Contains(l.Why, "DROP") {
		t.Fatalf("superuser owner line = %+v, want reassign advice and no grant", l)
	}
}

// Invalid input: an owner role name that needs quoting is quoted.
func TestOwnershipOwnerRoleIsQuoted(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.Owners = []TableOwner{{Role: `we"ird`, Tables: 1}}
	in.Grants.OwnerRoles = 1
	lines := ownerLines(level(TrustGuide(in), LevelAdvisory))
	if len(lines) != 1 || lines[0].SQL != `GRANT "we""ird" TO sage_agent;` {
		t.Fatalf("owner lines = %+v, want the quoted role", lines)
	}
}

// Boundary: more owner roles than the guide lists says how many are left.
func TestOwnershipMoreOwnersThanListed(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.Owners = []TableOwner{{Role: "o1", Tables: 5}, {Role: "o2", Tables: 4}}
	in.Grants.OwnerRoles = 23
	lines := ownerLines(level(TrustGuide(in), LevelAdvisory))
	if len(lines) != 2 || !strings.Contains(lines[1].Detail, "21 more owner roles") {
		t.Fatalf("lines = %+v, want the last naming 21 more owner roles", lines)
	}
}

// Nil/empty: no user tables gives one ownership grant, unknown, with no
// owner lines and no placeholder SQL.
func TestOwnershipNoUserTables(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.TablesOwned, in.Grants.TablesTotal = 0, 0
	l := level(TrustGuide(in), LevelAdvisory)
	if len(ownerLines(l)) != 0 {
		t.Fatalf("owner lines = %+v, want none", ownerLines(l))
	}
	g := grant(l, GrantTableOwnership)
	if g.Name == "" || g.Present != nil || strings.Contains(g.SQL, "GRANT") {
		t.Fatalf("ownership = %+v, want one unknown grant without a GRANT", g)
	}
}

func TestOwnershipAllOwnedIsPresent(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.TablesOwned, in.Grants.TablesTotal = 10, 10
	l := level(TrustGuide(in), LevelAdvisory)
	all := ownershipAny(l)
	if len(all) != 1 || all[0].Present == nil || !*all[0].Present ||
		strings.Contains(all[0].SQL, "GRANT") {
		t.Fatalf("ownership = %+v, want one present grant without a GRANT", all)
	}
}

// No grant in any level hands out a placeholder in angle brackets.
func TestTrustGuideHasNoPlaceholderSQL(t *testing.T) {
	inputs := []GuideInput{guideInput(LevelObservation), {Current: LevelObservation}}
	withOwners := guideInput(LevelObservation)
	withOwners.Grants.Owners = []TableOwner{{Role: "postgres", Tables: 1, Superuser: true},
		{Role: "app", Tables: 2}}
	withOwners.Grants.Maintain = bp(true)
	inputs = append(inputs, withOwners)
	for _, in := range inputs {
		for _, l := range TrustGuide(in) {
			for _, g := range l.Grants {
				if strings.ContainsAny(g.SQL, "<>") {
					t.Fatalf("%s grant %s SQL %q has a placeholder", l.Level, g.Name, g.SQL)
				}
			}
		}
	}
}

// The signal grant uses the shared probe's SQL: a member that does not
// inherit pg_signal_backend gets the fix for that, not a no-op GRANT.
func TestSignalGrantForMemberWithoutInherit(t *testing.T) {
	in := guideInput(LevelObservation)
	in.Grants.Signal = &rolegrants.SignalBackend{MemberOnly: true, Version: 170000}
	g := grant(level(TrustGuide(in), LevelAdvisory), GrantSignalBackend)
	if g.SQL != "GRANT pg_signal_backend TO sage_agent WITH INHERIT TRUE;" {
		t.Fatalf("signal grant = %+v, want the WITH INHERIT TRUE fix", g)
	}
	if !strings.Contains(g.Detail, "inherit") {
		t.Fatalf("signal detail %q, want it to say the membership is not inherited",
			g.Detail)
	}
}
