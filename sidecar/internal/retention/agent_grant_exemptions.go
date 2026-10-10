package retention

// agentGrantExemptions: the grant registry is the record of who held which
// privilege and when (AGENTDB-SPEC §6.6, §6.17), and a decided capability
// request is the approval behind a grant.
var agentGrantExemptions = map[string]string{
	"guard_grants": "the agent grant registry: one row per grant, kept as the " +
		"evidence of who held which privilege and who revoked it",
	"guard_grant_requests": "agent capability requests and their decisions, the " +
		"approval record behind each grant; bounded by the per-agent request rate",
}
