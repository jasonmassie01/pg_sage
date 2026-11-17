package api

// readRouteScopes declares the read scope (apiauth-08b, route_read.go) of
// each GET route registered with route() behind a viewer or operator gate.
// Routes whose handlers aggregate through the fleet register with
// routeAggregate instead and are not listed. Admin GETs are admin-only.
// A route marked fleet-only reads data this release cannot narrow to a
// database set (process-wide state, agent principals, tables keyed by
// something other than a database): a user scoped by grants is refused
// rather than shown other databases' data. Each change here is reviewed
// with testdata/route_scopes.golden.
var readRouteScopes = map[string]readScope{
	// No database data: the caller's own identity and static lists.
	"GET /api/v1/auth/me":             readGlobal,
	"GET /api/v1/auth/sso":            readGlobal,
	"GET /api/v1/llm/models":          readGlobal,
	"GET /api/v1/findings/categories": readGlobal,

	// One database, named in the path.
	"GET /api/v1/databases/{db}/ask/budget":             readDatabase(pathDB),
	"GET /api/v1/databases/{db}/ask/conversations":      readDatabase(pathDB),
	"GET /api/v1/databases/{db}/ask/conversations/{id}": readDatabase(pathDB),
	"GET /api/v1/databases/{db}/ask/runs/{id}":          readDatabase(pathDB),
	"GET /api/v1/databases/{db}/facts/peak-suggestion":  readDatabase(pathDB),
	"GET /api/v1/databases/{db}/investigations":         readDatabase(pathDB),
	// evid-02a: a database's workload evidence packs.
	"GET /api/v1/databases/{db}/workload-packs":                        readDatabase(pathDB),
	"GET /api/v1/databases/{db}/workload-packs/{sha}":                  readDatabase(pathDB),
	"GET /api/v1/databases/{db}/workload-packs/{sha}/evidence/{id...}": readDatabase(pathDB),
	"GET /api/v1/databases/{db}/workload-packs/{sha}/export":           readDatabase(pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}":                   readDatabase(pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}/events": readDatabase(
		pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}/evidence/{evidence}": readDatabase(
		pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}/export": readDatabase(
		pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}/maintenance": readDatabase(
		pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}/proposals": readDatabase(
		pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}/replay-case": readDatabase(
		pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}/similar": readDatabase(
		pathDB),
	"GET /api/v1/databases/{db}/investigations/{id}/transcript": readDatabase(
		pathDB),
	"GET /api/v1/databases/{db}/runbooks":                 readDatabase(pathDB),
	"GET /api/v1/databases/{db}/runbooks/{id}":            readDatabase(pathDB),
	"GET /api/v1/databases/{db}/runbooks/{id}/runs":       readDatabase(pathDB),
	"GET /api/v1/agent-classifications/{database}":        readDatabase(classDB),
	"GET /api/v1/agent-classifications/{database}/column": readDatabase(classDB),
	"GET /api/v1/agent-environments/{database}":           readDatabase(classDB),

	// Process-wide or not narrowable to a database set.
	"GET /api/v1/config":                        readFleetOnly,
	"GET /api/v1/llm/status":                    readFleetOnly,
	"GET /api/v1/llm/usage":                     readFleetOnly,
	"GET /api/v1/lakebase-observability/status": readFleetOnly,
	"GET /api/v1/onboarding":                    readFleetOnly,
	"GET /api/v1/onboarding/trust":              readFleetOnly,
	"GET /api/v1/first-look":                    readFleetOnly,
	"GET /api/v1/model-lift":                    readFleetOnly,
	"GET /api/v1/policy":                        readFleetOnly,
	"GET /api/v1/policy/history":                readFleetOnly,
	"GET /api/v1/trust":                         readFleetOnly,
	"GET /api/v1/upgrade-readiness":             readFleetOnly,
	"GET /api/v1/case-tickets":                  readFleetOnly,
	"GET /api/v1/notifications/log":             readFleetOnly,
	"GET /api/v1/fleet/lookalikes":              readFleetOnly,
	"GET /api/v1/fleet/leader":                  readFleetOnly,
	"GET /api/v1/sre/autonomy":                  readFleetOnly,
	"GET /api/v1/sre/autonomy/history":          readFleetOnly,
	"GET /api/v1/sre/autonomy/proposals":        readFleetOnly,
	"GET /api/v1/sre/autonomy/game-days":        readFleetOnly,
	"GET /api/v1/sre/autonomy/bench-runs":       readFleetOnly,
	"GET /api/v1/sre/autonomy/rollouts":         readFleetOnly,
	"GET /api/v1/sre/autonomy/rollouts/{id}":    readFleetOnly,
	"GET /api/v1/agents":                        readFleetOnly,
	"GET /api/v1/agents/freezes":                readFleetOnly,
	"GET /api/v1/agents/{id}":                   readFleetOnly,
	"GET /api/v1/agents/{id}/activity":          readFleetOnly,
	"GET /api/v1/agents/{id}/changes":           readFleetOnly,
	"GET /api/v1/agents/{id}/grant-requests":    readFleetOnly,
	"GET /api/v1/agents/{id}/grants":            readFleetOnly,
	"GET /api/v1/specialist-requests":           readFleetOnly,
	"GET /api/v1/findings/{id}/pending-actions": readFleetOnly,
	"GET /api/v1/actions/pending":               readFleetOnly, // standalone router
	"GET /api/v1/actions/pending/count":         readFleetOnly, // standalone router
}
