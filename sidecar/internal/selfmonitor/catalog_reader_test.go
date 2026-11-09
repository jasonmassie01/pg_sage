package selfmonitor

import "testing"

// footprint-06: reading a statistics view is not pg_sage's signature. A
// Datadog, pganalyze or pgwatch agent reads the same views, so an untagged
// catalog read is not pg_sage's own; only the tag (or a sage. reference)
// is. The earlier version of this test asserted the over-match.
// No concurrent access tests: IsQueryText is a pure function.
func TestIsQueryText_CatalogReadersAreNotSelf(t *testing.T) {
	monitorQueries := []string{
		// Datadog-style statement metrics read.
		"SELECT pg_stat_statements.userid, pg_stat_statements.dbid, " +
			"pg_stat_statements.queryid, calls, total_exec_time " +
			"FROM pg_stat_statements LEFT JOIN pg_roles ON " +
			"pg_stat_statements.userid = pg_roles.oid LIMIT $1",
		// pgwatch-style session count.
		"select datname, state, count(*) from pg_stat_activity " +
			"where pid != pg_backend_pid() group by 1, 2",
		"SELECT schemaname, relname FROM pg_stat_user_tables WHERE n_dead_tup > 0",
		"SELECT indexrelname FROM pg_stat_user_indexes",
		"select * from pg_stat_replication",
		"SELECT * FROM pg_stat_database WHERE datname = current_database()",
		"SELECT * FROM pg_stat_bgwriter",
	}
	for _, q := range monitorQueries {
		if IsQueryText(q) {
			t.Errorf("a monitor's catalog read is classified as pg_sage's: %.70s", q)
		}
	}
}

// The tag identity is unchanged: pg_sage's own catalog reads carry the tag
// (or name the sage schema) and stay self.
func TestIsQueryText_TaggedCatalogReadersStaySelf(t *testing.T) {
	selfQueries := []string{
		"/* pg_sage */ SELECT queryid, calls FROM pg_stat_statements",
		"/* pg_sage collector */ select state, count(*) from pg_stat_activity group by 1",
		"SELECT s.relname FROM pg_stat_user_tables s JOIN sage.snapshots x ON true",
	}
	for _, q := range selfQueries {
		if !IsQueryText(q) {
			t.Errorf("a tagged pg_sage statement is not recognized: %.70s", q)
		}
	}
	userQueries := []string{
		"SELECT id, name FROM public.customers WHERE active = true",
		"UPDATE orders SET status = 'shipped' WHERE id = $1",
		"SELECT count(*) FROM events WHERE created_at > now() - interval '1 day'",
		"",
		"   ",
	}
	for _, q := range userQueries {
		if IsQueryText(q) {
			t.Errorf("false positive on user query: %q", q)
		}
	}
}
