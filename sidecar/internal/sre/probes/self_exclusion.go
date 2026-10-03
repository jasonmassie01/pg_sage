package probes

import "github.com/pg-sage/sidecar/internal/selfmonitor"

// notSelf is the pg_stat_activity predicate (alias a) that leaves
// pg_sage's own sessions out of a probe that judges the workload (perf
// v1.8.3, perf-selfexcl): lock waiters, open transactions, active
// backends. Probes that show what holds a resource (lock blockers, xmin
// holders, temp file holders, connection slots) keep pg_sage visible: its
// cost is real and the action layer protects its sessions.
func notSelf(alias string) string { return selfmonitor.ActivityExclusionSQL(alias) }

// notSelfStatement is the pg_stat_statements predicate on the text column
// that leaves pg_sage's own (tagged) statements out.
func notSelfStatement(column string) string {
	return selfmonitor.StatementExclusionSQL(column)
}
