package api

// fleetWideRoutes lists the operator writes that do not act on one
// database (apiauth-08), each with the reason. route() registers every
// operator write as fleet-wide unless it uses routeDB; with
// api.database_grants on, a database-scoped operator cannot use these
// (they act as a viewer here), only an operator with no grants or an
// admin. A new operator write that names a database registers with
// routeDB instead, or the registry test fails.
var fleetWideRoutes = map[string]string{
	"POST /api/v1/agents/{id}/freeze": "an agent principal spans every database it " +
		"can reach; freezing it is a fleet decision",
	"POST /api/v1/first-look/recheck": "re-runs the first-look probes of the " +
		"deployment, not of one chosen database",
	"POST /api/v1/policy/proposals": "a standing policy proposal can cover several " +
		"databases or the whole fleet",
	"POST " + bulkApprovePath: "one bulk approval can name cards on several databases " +
		"(ux-07b); a database-scoped operator approves each card on its own database",
}
