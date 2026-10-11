package decommission

// InventoryPath and AckPath are the §12 endpoints (AGENTDB-SPEC §8.3).
const (
	InventoryPath = "/api/v1/agentdb/decommission"
	AckPath       = InventoryPath + "/ack"
)

// AckTable is the sage table that records acknowledgements.
const AckTable = "agentdb_decommission"

// Route is one method and path of the removed provisioning API, with ids
// filled in, so the router tests can prove each answers 404.
type Route struct{ Method, Path string }

const (
	dbs   = "/api/v1/agent-dbs"
	dep   = dbs + "/dep-1"
	agent = "/api/v1/agent-api/"
)

// RemovedRoutes is the provisioning API that G0 removed (the manifest's
// internal/api/agent_db_* files and their sub-router).
var RemovedRoutes = []Route{
	{"GET", dbs}, {"POST", dbs}, {"POST", dbs + "/cleanup"},
	{"GET", dbs + "/requests"}, {"POST", dbs + "/requests"},
	{"GET", dbs + "/requests/r1"}, {"POST", dbs + "/requests/r1/approve"},
	{"POST", dbs + "/requests/r1/deny"}, {"POST", dbs + "/requests/r1/provision"},
	{"GET", dbs + "/providers"}, {"GET", dbs + "/provider-configs"},
	{"POST", dbs + "/provider-configs/aws_rds"},
	{"GET", dbs + "/terraform-templates"}, {"POST", dbs + "/terraform-templates"},
	{"POST", dbs + "/terraform-templates/t1/approve"},
	{"POST", dbs + "/terraform-templates/t1/provision"},
	{"GET", dbs + "/blueprints"}, {"POST", dbs + "/blueprints"},
	{"POST", dbs + "/blueprints/b1/approve"}, {"POST", dbs + "/blueprints/b1/provision"},
	{"GET", dbs + "/identities"}, {"POST", dbs + "/identities"},
	{"POST", dbs + "/identities/a1/tokens"}, {"POST", dbs + "/reconcile"},
	{"GET", dbs + "/size-profiles"}, {"POST", dbs + "/size-profiles"},
	{"DELETE", dbs + "/size-profiles/p1"},
	{"GET", dep}, {"DELETE", dep}, {"POST", dep + "/ping"},
	{"POST", dep + "/agent-ping"}, {"POST", dep + "/extend-lease"},
	{"GET", dep + "/recommendations"}, {"POST", dep + "/recommendations"},
	{"POST", dep + "/recommendations/rec1/feedback"},
	{"GET", dep + "/audit"}, {"GET", dep + "/audit/export"},
	{"GET", dep + "/deploy-requests"}, {"POST", dep + "/deploy-requests"},
	{"GET", dep + "/deploy-requests/dr1"},
	{"POST", dep + "/deploy-requests/dr1/request-review"},
	{"POST", dep + "/deploy-requests/dr1/approve"},
	{"POST", dep + "/deploy-requests/dr1/deny"},
	{"GET", dep + "/ping-tokens"}, {"POST", dep + "/ping-tokens"},
	{"POST", dep + "/ping-tokens/pt1/rotate"}, {"POST", dep + "/ping-tokens/pt1/revoke"},
	{"POST", dep + "/cost-samples"}, {"GET", dep + "/cost"},
	{"GET", dep + "/backups"}, {"POST", dep + "/backups"},
	{"POST", dep + "/backups/restore-drill"}, {"POST", dep + "/backups/check"},
	{"POST", dep + "/backups/restore-drill-dry-run"}, {"GET", dep + "/tuning-hints"},
	{"POST", dep + "/provision/preflight"}, {"POST", dep + "/provision/authorize-live"},
	{"POST", dep + "/provision/execute"}, {"POST", dep + "/provision/status"},
	{"POST", dep + "/provision/destroy-dry-run"}, {"POST", dep + "/provision/destroy-live"},
	{"GET", dep + "/provision/attempts"}, {"GET", dep + "/cleanup"},
	{"POST", dep + "/archive"}, {"POST", dep + "/restore"},
	{"GET", agent + "agent-dbs"}, {"GET", agent + "agent-dbs/dep-1"},
	{"POST", agent + "agent-db-requests"}, {"GET", agent + "agent-db-requests"},
}

// LegacyTables are the 27 tables the removed provisioner created. They stay
// in G0 so the inventory can read them; G1 drops them by this explicit list
// (§12 step 5).
var LegacyTables = []string{
	"agent_identities", "agent_db_requests", "agent_db_deployments",
	"agent_db_provider_configs", "agent_db_creation_receipts",
	"agent_db_terraform_templates", "agent_db_blueprints", "agent_db_size_profiles",
	"agent_db_pings", "agent_db_ping_tokens", "agent_db_ping_token_failures",
	"agent_db_recommendations", "agent_db_cost_samples", "agent_db_backups",
	"agent_db_tuning_hints", "agent_db_provision_attempts", "agent_db_audit",
	"agent_db_deploy_requests", "agent_db_live_plans", "agent_db_live_estimates",
	"agent_db_live_authorizations", "agent_db_live_receipts",
	"agent_db_monitoring_policies", "agent_db_monitoring_state",
	"agent_db_monitoring_work", "agent_db_agent_tokens", "agent_db_schema_version",
}
