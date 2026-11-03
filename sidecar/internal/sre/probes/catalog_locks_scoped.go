package probes

import (
	"strings"
)

// Retain local waiters and local blocker evidence. A blocker outside this
// database supplies only a boolean denial, never its PID or session fields.
// The runner turns that sentinel into unavailable evidence, not healthy absence.
var databaseLockGraphSQL = func() string {
	s := strings.ReplaceAll(lockGraphSQL, "bp.pid AS blocker_pid", "CASE WHEN bp.pid = 0 THEN 0 ELSE b.pid END AS blocker_pid")
	s = strings.ReplaceAll(s, "LEFT JOIN pg_catalog.pg_stat_activity b ON b.pid = bp.pid AND bp.pid <> 0", "LEFT JOIN pg_catalog.pg_stat_activity b ON b.pid = bp.pid AND bp.pid <> 0 AND b.datname = pg_catalog.current_database()")
	s = strings.ReplaceAll(s, "b.query_id AS blocker_query_id", "b.query_id AS blocker_query_id, (bp.pid <> 0 AND b.pid IS NULL) AS _outside_scope")
	return s
}()
