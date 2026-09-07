package selfmonitor

import "testing"

// TestIsQueryText_CollectorCatalogReaders covers detection of pg_sage's own
// statistics-catalog reads that carry no /* pg_sage */ tag and no sage.
// schema reference (historical findings captured before tagging).
func TestIsQueryText_CollectorCatalogReaders(t *testing.T) {
	selfQueries := []string{
		"SELECT COALESCE(queryid, $1), query, calls FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())",
		"SELECT schemaname, relname FROM pg_stat_user_tables WHERE n_dead_tup > 0",
		"SELECT indexrelname FROM pg_stat_user_indexes",
		"SELECT state, count(*) FROM pg_stat_activity GROUP BY state",
		"select * from pg_stat_replication",
	}
	for _, q := range selfQueries {
		if !IsQueryText(q) {
			t.Errorf("expected self-monitoring detection for: %.60s", q)
		}
	}
	// A user's application query that does NOT read pg_sage's catalogs.
	userQueries := []string{
		"SELECT id, name FROM public.customers WHERE active = true",
		"UPDATE orders SET status = 'shipped' WHERE id = $1",
		"SELECT count(*) FROM events WHERE created_at > now() - interval '1 day'",
	}
	for _, q := range userQueries {
		if IsQueryText(q) {
			t.Errorf("false positive on user query: %.60s", q)
		}
	}
}
