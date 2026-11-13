package schema

import "testing"

// market-09: sage.provider_recommendations and sage.provider_advice_sources
// exist after bootstrap, bootstrap runs twice cleanly, one row is kept per
// (database, provider, provider_id), and the verdict and kind are checked.
func TestProviderAdviceMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.provider_recommendations
			WHERE database = 'migration_probe'`)
	})
	insert := `INSERT INTO sage.provider_recommendations
		(database, provider, provider_id, kind, payload, verdict, reason)
		VALUES ('migration_probe', 'azure', $1, $2, '{}', $3, 'probe')`
	if _, err := pool.Exec(ctx, insert, "a", "create_index", "agree"); err != nil {
		t.Fatalf("valid row rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "a", "create_index", "agree"); err == nil {
		t.Fatal("a second row for one provider id was accepted")
	}
	for _, bad := range [][2]string{{"parameter_x", "agree"}, {"drop_index", "maybe"}} {
		if _, err := pool.Exec(ctx, insert, "b", bad[0], bad[1]); err == nil {
			t.Fatalf("kind %q verdict %q accepted", bad[0], bad[1])
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_attribute
		WHERE attrelid = 'sage.provider_advice_sources'::regclass AND attnum > 0
		  AND attname IN ('database', 'provider', 'last_attempt', 'last_success',
		                  'last_error') AND NOT attisdropped`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("provider_advice_sources columns = %d (%v), want 5", n, err)
	}
}
