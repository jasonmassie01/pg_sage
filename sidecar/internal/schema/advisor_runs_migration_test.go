package schema

import "testing"

// advisors-02: sage.advisor_runs exists after bootstrap, bootstrap runs
// twice cleanly, and its key is (database_name, sub_advisor) so fleet
// databases sharing a sage schema never share a schedule.
func TestAdvisorRunsMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var key string
	err := pool.QueryRow(ctx, `SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		FROM pg_index i
		CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = 'sage.advisor_runs'::regclass AND i.indisprimary`).Scan(&key)
	if err != nil {
		t.Fatalf("reading the sage.advisor_runs key: %v", err)
	}
	if key != "database_name,sub_advisor" {
		t.Fatalf("primary key = %q, want database_name,sub_advisor", key)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.advisor_runs (sub_advisor,
		consecutive_failures) VALUES ('migration_probe', -1)`)
	if err == nil {
		t.Fatal("negative consecutive_failures accepted")
	}
}
