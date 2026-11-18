package retention

// upgradeBriefExemptions: the upgrade brief's history is delta-only
// (drill-02a): a row is added only when a database's brief changes, so it
// grows with the operator's upgrade work, not with time, and the latest
// row is the brief's age.
var upgradeBriefExemptions = map[string]string{
	"upgrade_briefs": "delta-only: a row only when a database's upgrade brief changes; " +
		"the latest row dates the brief, so none is aged out",
}
