package onboarding

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// Grant names in the trust guide.
const (
	GrantMonitor        = "pg_monitor"
	GrantSageSchema     = "sage_schema"
	GrantTableOwnership = "table_ownership"
	GrantSchemaCreate   = "schema_create"
	GrantMaintain       = "pg_maintain"
	GrantSignalBackend  = "pg_signal_backend"
	GrantAlterSystem    = "alter_system"
)

// Trust levels, from read-only up.
const (
	LevelObservation = "observation"
	LevelAdvisory    = "advisory"
	LevelAutonomous  = "autonomous"
)

// Grant is one privilege a trust level needs; Present is nil when it was
// not checked.
type Grant struct {
	Name    string `json:"name"`
	Why     string `json:"why"`
	SQL     string `json:"sql"`
	Present *bool  `json:"present"`
	Detail  string `json:"detail,omitempty"`
}

// Level is what one trust level allows, never does, waits for and needs.
type Level struct {
	Level   string   `json:"level"`
	Title   string   `json:"title"`
	Current bool     `json:"current"`
	Allows  []string `json:"allows"`
	Never   []string `json:"never"`
	Waits   []string `json:"waits"`
	Grants  []Grant  `json:"grants"`
}

// GuideInput is the configuration and grants the guide explains.
type GuideInput struct {
	Current           string
	ExecutionMode     string
	SafeRampHours     float64
	ModerateRampHours float64
	Tier3Safe         bool
	Tier3Moderate     bool
	// RampElapsedHours is how long the trust ramp has run; nil if unknown.
	RampElapsedHours *float64
	Grants           GrantStatus
}

// TrustGuide explains the three trust levels for the "grant more" step.
func TrustGuide(in GuideInput) []Level {
	role := quoteRole(in.Grants.Role)
	levels := []Level{observationLevel(in, role), advisoryLevel(in, role),
		autonomousLevel(in, role)}
	for i := range levels {
		levels[i].Current = levels[i].Level == in.Current
	}
	return levels
}

func observationLevel(in GuideInput, role string) Level {
	g := in.Grants
	return Level{Level: LevelObservation, Title: "Observe only (read-only)",
		Allows: []string{"Read the system catalog and the statistics views.",
			"Record findings, the first look and the actions it would take (shadow " +
				"decisions) in its own sage schema."},
		Never: []string{"Change anything outside the sage schema: no DDL, no VACUUM or " +
			"ANALYZE, no settings, no cancelled queries."},
		Waits: []string{},
		Grants: []Grant{
			{Name: GrantMonitor, Why: "Read statistics, settings and query text.",
				SQL: "GRANT pg_monitor TO " + role + ";", Present: g.Monitor},
			{Name: GrantSageSchema, Why: "Keep pg_sage's own state in its sage schema.",
				SQL:     "CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION " + role + ";",
				Present: g.SageSchema},
		}}
}

func advisoryLevel(in GuideInput, role string) Level {
	l := Level{Level: LevelAdvisory, Title: "Safe changes",
		Allows: []string{"Read-only actions as soon as they are proposed.",
			fmt.Sprintf("Eligible typed SAFE actions after the %.0f h safe ramp, "+
				"verified afterwards and reverted when they regress.", in.SafeRampHours)},
		Never: []string{"MODERATE actions run only after a person approves them.",
			"High-risk actions run only after a person approves them."},
		Waits:  rampWaits(in.RampElapsedHours, in.SafeRampHours, "safe"),
		Grants: ownershipGrants(in.Grants, role)}
	if !in.Tier3Safe {
		l.Waits = append(l.Waits, "trust.tier3_safe is off: SAFE actions also wait for "+
			"approval.")
	}
	return withExecutionMode(l, in.ExecutionMode)
}

func autonomousLevel(in GuideInput, role string) Level {
	l := Level{Level: LevelAutonomous, Title: "Earned autonomy",
		Allows: []string{"Everything advisory allows.",
			fmt.Sprintf("Eligible typed MODERATE actions after the %.0f h moderate ramp, "+
				"inside the maintenance window.", in.ModerateRampHours)},
		Never: []string{"High-risk actions run only after a person approves them, at " +
			"every level."},
		Waits: rampWaits(in.RampElapsedHours, in.ModerateRampHours, "moderate"),
		Grants: append(ownershipGrants(in.Grants, role), Grant{Name: GrantAlterSystem,
			Why: "Change server settings (managed services: the parameter group or flags).",
			SQL: "GRANT ALTER SYSTEM ON PARAMETER work_mem TO " + role +
				"; -- one grant per parameter (PostgreSQL 15+)",
			Present: in.Grants.AlterSystem})}
	if !in.Tier3Moderate {
		l.Waits = append(l.Waits, "trust.tier3_moderate is off: MODERATE actions wait "+
			"for approval until it is turned on.")
	}
	return withExecutionMode(l, in.ExecutionMode)
}

func withExecutionMode(l Level, mode string) Level {
	if mode != "" && mode != "auto" {
		l.Waits = append(l.Waits, fmt.Sprintf("execution_mode is %s: every action waits "+
			"for approval.", mode))
	}
	return l
}

// rampWaits states how much of a trust ramp is left.
func rampWaits(elapsed *float64, ramp float64, kind string) []string {
	switch {
	case elapsed == nil:
		return []string{fmt.Sprintf("Waits for the %.0f h %s ramp from the install's "+
			"trust ramp start.", ramp, kind)}
	case *elapsed < ramp:
		return []string{fmt.Sprintf("%.0f h left of the %.0f h %s ramp.", ramp-*elapsed,
			ramp, kind)}
	}
	return []string{}
}

func ownershipGrants(g GrantStatus, role string) []Grant {
	owned := Grant{Name: GrantTableOwnership,
		Why: "CREATE INDEX and other DDL need the privileges of the table owner.",
		SQL: "GRANT <table owner role> TO " + role + ";"}
	if g.TablesTotal > 0 {
		all := g.TablesOwned == g.TablesTotal
		owned.Present = &all
		owned.Detail = fmt.Sprintf("owns %d of %d tables", g.TablesOwned, g.TablesTotal)
	}
	out := []Grant{owned, schemaCreateGrant(g.SchemaCreate, role)}
	if g.Maintain != nil {
		out = append(out, Grant{Name: GrantMaintain,
			Why: "VACUUM, ANALYZE and REINDEX tables it does not own (PostgreSQL 17+).",
			SQL: "GRANT pg_maintain TO " + role + ";", Present: g.Maintain})
	}
	return append(out, Grant{Name: GrantSignalBackend,
		Why: "Cancel a runaway query when an approved action needs it.",
		SQL: "GRANT pg_signal_backend TO " + role + ";", Present: g.SignalBackend})
}

// schemaCreateGrant is CREATE on the schemas holding user tables, with the
// same SQL the startup check prints (rolegrants). It is unknown when it was
// not checked or no user table exists yet.
func schemaCreateGrant(sc *rolegrants.SchemaCreate, role string) Grant {
	var s rolegrants.SchemaCreate
	if sc != nil {
		s = *sc
	}
	out := Grant{Name: GrantSchemaCreate,
		Why: "CREATE INDEX and CREATE STATISTICS also need CREATE on the table's schema, " +
			"even for the table's owner.",
		SQL: s.GrantSQL(role) + ";"}
	if len(s.Schemas) == 0 {
		return out
	}
	ok := !s.Lacking()
	out.Present = &ok
	out.Detail = fmt.Sprintf("CREATE on %d of %d schemas with tables",
		len(s.Schemas)-len(s.Missing), len(s.Schemas))
	if !ok {
		out.Detail += "; missing: " + nameSome(s.Missing)
	}
	return out
}

// detailSchemaNames is how many missing schemas a grant's one-line detail
// names; the SQL names every one.
const detailSchemaNames = 10

// nameSome lists the first detailSchemaNames names and counts the rest.
func nameSome(names []string) string {
	if len(names) <= detailSchemaNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:detailSchemaNames], ", "),
		len(names)-detailSchemaNames)
}

// quoteRole quotes a role name the way the startup check does; an unknown
// role reads as the documented sage_agent.
func quoteRole(role string) string {
	if role == "" {
		return "sage_agent"
	}
	return rolegrants.QuoteRole(role)
}
