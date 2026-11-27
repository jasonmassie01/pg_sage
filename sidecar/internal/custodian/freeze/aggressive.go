package freeze

// AggressiveAges are the table ages at which a plain VACUUM of a table is
// already aggressive (vacuum-05): it then visits every page that is not
// all-frozen and advances relfrozenxid and relminmxid, so the custodian
// needs no FREEZE, which rewrites every page and writes far more WAL.
// Known is false when the server settings were not read: the custodian then
// keeps VACUUM (FREEZE), which advances the horizon whatever the settings.
type AggressiveAges struct {
	XID       int64
	MultiXact int64
	Known     bool
}

// AggressiveVacuumAges derives the effective ages the way VACUUM does:
// vacuum_freeze_table_age capped at 95% of autovacuum_freeze_max_age, and
// the multixact pair likewise. A zero table age is valid (every VACUUM is
// aggressive); a negative table age or a non-positive maximum is unknown.
func AggressiveVacuumAges(
	xidTableAge, xidMaxAge, mxidTableAge, mxidMaxAge int64,
) AggressiveAges {
	if xidTableAge < 0 || mxidTableAge < 0 || xidMaxAge <= 0 || mxidMaxAge <= 0 {
		return AggressiveAges{}
	}
	return AggressiveAges{XID: min(xidTableAge, int64(float64(xidMaxAge)*0.95)),
		MultiXact: min(mxidTableAge, int64(float64(mxidMaxAge)*0.95)), Known: true}
}

// VacuumSQL is the custodian's VACUUM of one table: plain when the table's
// XID or multixact age is at or above its aggressive age, VACUUM (FREEZE)
// otherwise or when the ages are unknown.
func VacuumSQL(schema, table string, xidAge, mxidAge int64, at AggressiveAges) string {
	if at.Known && (xidAge >= at.XID || mxidAge >= at.MultiXact) {
		return "VACUUM " + quoteIdent(schema) + "." + quoteIdent(table)
	}
	return VacuumFreezeSQL(schema, table)
}
