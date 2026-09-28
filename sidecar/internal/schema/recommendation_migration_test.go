package schema

import (
	"context"
	"testing"
)

var recommendationTables = []string{
	"recommendation", "recommendation_revision", "recommendation_transition",
}

func TestRecommendationTablesAreExpected(t *testing.T) {
	registered := map[string]bool{}
	for _, tbl := range expectedTables {
		registered[tbl.name] = true
	}
	for _, name := range recommendationTables {
		if !registered[name] {
			t.Errorf("sage.%s is not in expectedTables", name)
		}
	}
}

// The recommendation migration is additive and idempotent: an install
// without the tables gains them, and existing queue rows keep their data.
func TestRecommendationMigrationIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for i := len(recommendationTables) - 1; i >= 0; i-- {
		if _, err := pool.Exec(ctx,
			"DROP TABLE IF EXISTS sage."+recommendationTables[i]+" CASCADE"); err != nil {
			t.Fatalf("drop %s: %v", recommendationTables[i], err)
		}
	}
	var queueID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk) VALUES ('VACUUM x', 'safe') RETURNING id`).
		Scan(&queueID); err != nil {
		t.Fatalf("queue row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_queue WHERE id=$1",
			queueID)
	})
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, tbl := range recommendationTables {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema='sage' AND table_name=$1`, tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("sage.%s after two upgrades: count=%d err=%v", tbl, n, err)
		}
	}
	var sql string
	var recID *int64
	if err := pool.QueryRow(ctx, `SELECT proposed_sql, recommendation_id
		FROM sage.action_queue WHERE id=$1`, queueID).Scan(&sql, &recID); err != nil ||
		sql != "VACUUM x" || recID != nil {
		t.Fatalf("queue row after upgrade: sql=%q rec=%v err=%v", sql, recID, err)
	}
}

func TestRecommendationStateCheckRejectsUnknownState(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	_, err := pool.Exec(ctx, `INSERT INTO sage.recommendation
		(identity_key, database_name, category, target, action_type, state, revision,
		 content_hash, retry_budget)
		VALUES ('k', 'd', 'c', 't', 'a', 'bogus', 1, 'h', 0)`)
	if err == nil {
		t.Fatal("an unknown recommendation state was stored")
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.recommendation
		(identity_key, database_name, category, target, action_type, state, revision,
		 content_hash, retry_budget)
		VALUES ('k2', 'd', 'c', 't', 'a', 'approved', 1, 'h', 0)`)
	if err == nil {
		t.Fatal("an approved recommendation without a pinned approval was stored")
	}
}
