package schema

import "strings"

// API list indexes (perf v1.8.3, claude/perf-api). The findings and
// actions lists page by keyset, newest or most severe first, with id as
// the last tie-breaker; each index matches one list's filter and order so
// a page reads about a page of index entries, never the whole history:
//
//   - idx_findings_list_severity / idx_findings_list_last_seen: the
//     dashboard's findings sorts (by severity rank, by last seen) within a
//     status. The rank expression is the API's sevRankSQL, verbatim.
//   - idx_action_log_time_id, idx_action_queue_ledger: the actions ledger
//     (executed actions and not-yet-executed proposals) by time.
//   - idx_action_log_sql_md5: attempts per SQL statement, counted for the
//     rows of a page (it replaced a window over all of action_log).
//
// Same procedure as the decision ledger migration: each index is checked
// in the catalog first (a re-run takes no lock), an INVALID one is
// rebuilt, and a build runs under the bootstrap lock, once, pausing only
// pg_sage's own writes to that table. Idempotent.
var apiListIndexes = []ledgerIndex{
	{"idx_findings_list_severity", "findings", "INDEX %I ON sage.findings (status, " +
		"(CASE severity WHEN 'critical' THEN 3 WHEN 'warning' THEN 2 " +
		"WHEN 'info' THEN 1 ELSE 0 END), last_seen, id)"},
	{"idx_findings_list_last_seen", "findings",
		"INDEX %I ON sage.findings (status, last_seen, id)"},
	{"idx_action_log_time_id", "action_log",
		"INDEX %I ON sage.action_log (executed_at, id)"},
	{"idx_action_log_sql_md5", "action_log",
		"INDEX %I ON sage.action_log (md5(sql_executed), executed_at)"},
	{"idx_action_queue_ledger", "action_queue", "INDEX %I ON sage.action_queue " +
		"(proposed_at, id) WHERE status <> 'executed' AND proposed_at IS NOT NULL"},
}

// ddlAPIListIndexes is the migration: the checked index loop over
// apiListIndexes.
func ddlAPIListIndexes() string {
	values := make([]string, 0, len(apiListIndexes))
	for _, index := range apiListIndexes {
		values = append(values, "("+sqlLiteral(index.name)+", "+sqlLiteral(index.table)+
			", "+sqlLiteral(index.definition)+")")
	}
	loop := strings.Replace(ddlDecisionLedgerIndexLoop, "%s",
		strings.Join(values, ",\n            "), 1)
	loop = strings.ReplaceAll(loop, "%%", "%")
	return "DO $$\nDECLARE spec record;\nBEGIN" + loop + "\nEND $$;"
}
