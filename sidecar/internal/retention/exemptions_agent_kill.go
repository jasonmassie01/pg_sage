package retention

// agentKillExemptions: the kill switch's records (spec §6.10).
// Kills, freezes and unfreeze requests are the audit of who stopped and
// who restored an agent; §6.17 sets no age retention for them, and they
// grow by one row per human action. In-flight rows are current state:
// each is deleted when its request ends.
var agentKillExemptions = map[string]string{
	"guard_kills": "one row per kill with its report: the audit of who stopped " +
		"agents and what the kill reached",
	"guard_freezes": "one row per freeze, cleared (never deleted) when lifted: " +
		"the audit of who froze and who unfroze",
	"guard_unfreeze_requests": "one row per two-person unfreeze request: the " +
		"audit of both approvers",
	"guard_inflight": "current state: a row lives only while its agent request " +
		"runs and is deleted when it ends",
}
