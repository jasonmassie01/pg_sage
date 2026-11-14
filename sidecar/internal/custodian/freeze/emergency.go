package freeze

import "strings"

// The emergency VACUUM profile (vacuum-05b): a red table whose custodian
// VACUUM the executor's watchdog capped EmergencyCapFailures times is
// frozen without index cleanup (usually most of a large table's VACUUM)
// and without the end-of-run truncation, which takes an ACCESS EXCLUSIVE
// lock. Index bloat then grows until the next normal VACUUM, which the
// custodian proposes once the table has left red. PostgreSQL's own
// failsafe (vacuum_failsafe_age) does the same only near 1.6 billion XIDs.
const (
	// EmergencyCapFailures is how many capped runs switch a red table to
	// the emergency profile.
	EmergencyCapFailures = 2
	// ProfileEmergency is the evidence and escalation value of the profile.
	ProfileEmergency = "emergency"
	// VacuumCapOutcome is the outcome the executor's watchdog gives a VACUUM
	// it cancelled at the cap; a stored failure reason starts with it and
	// a colon.
	VacuumCapOutcome = "vacuum_cap"
)

// emergencyOptions are the profile's VACUUM options (PostgreSQL 12+).
const emergencyOptions = "INDEX_CLEANUP OFF, TRUNCATE OFF"

// EmergencyVacuumSQL is VacuumSQL's plain-or-FREEZE choice with the
// emergency options.
func EmergencyVacuumSQL(schema, table string, xidAge, mxidAge int64, at AggressiveAges) string {
	target := quoteIdent(schema) + "." + quoteIdent(table)
	if at.Known && (xidAge >= at.XID || mxidAge >= at.MultiXact) {
		return "VACUUM (" + emergencyOptions + ") " + target
	}
	return "VACUUM (FREEZE, " + emergencyOptions + ") " + target
}

// CustodianVacuumStatements are every VACUUM the custodian builds for a
// table: plain, FREEZE and their emergency forms.
func CustodianVacuumStatements(schema, table string) []string {
	aggressive := AggressiveAges{Known: true}
	return []string{VacuumSQL(schema, table, 0, 0, aggressive), VacuumFreezeSQL(schema, table),
		EmergencyVacuumSQL(schema, table, 0, 0, aggressive),
		EmergencyVacuumSQL(schema, table, 0, 0, AggressiveAges{})}
}

// IsEmergencyVacuumSQL reports a VACUUM whose option list (never its
// target) turns index cleanup off.
func IsEmergencyVacuumSQL(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	rest, ok := strings.CutPrefix(upper, "VACUUM")
	if !ok {
		return false
	}
	rest, ok = strings.CutPrefix(strings.TrimSpace(rest), "(")
	if !ok {
		return false
	}
	options, _, ok := strings.Cut(rest, ")")
	if !ok {
		return false
	}
	for _, option := range strings.Split(options, ",") {
		if strings.Join(strings.Fields(option), " ") == "INDEX_CLEANUP OFF" {
			return true
		}
	}
	return false
}

// emergencyProfile reports a red table whose capped runs reached
// EmergencyCapFailures.
func emergencyProfile(input ResponseInput) bool {
	return input.Urgency == UrgencyRed && input.CapFailures >= EmergencyCapFailures
}
