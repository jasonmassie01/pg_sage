package retention

import (
	"context"
	"testing"
)

// insertRecommendation writes a head with one revision and one history
// row, last updated age ago.
func insertRecommendation(
	t *testing.T, ctx context.Context, tag, state, age string,
) int64 {
	t.Helper()
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	id := insertID(t, ctx, `INSERT INTO sage.recommendation
		(identity_key, database_name, category, target, action_type, state,
		 revision, content_hash, approved_revision, approved_hash, approved_by,
		 approved_at, retry_budget, created_at, updated_at, last_seen_at)
		VALUES (md5(random()::text), $1, 'retention', $2, 'create_index', $2, 1, $3,
		        CASE WHEN $2 = 'proposed' THEN NULL ELSE 1 END,
		        CASE WHEN $2 = 'proposed' THEN NULL ELSE $3 END,
		        CASE WHEN $2 = 'proposed' THEN NULL ELSE 'user:1' END,
		        CASE WHEN $2 = 'proposed' THEN NULL ELSE now() END,
		        2, now() - $4::interval, now() - $4::interval, now() - $4::interval)
		RETURNING id`, tag, state, hash, age)
	execRetry(t, ctx, `INSERT INTO sage.recommendation_revision
		(recommendation_id, revision, content_hash, forward_sql, source, created_at)
		VALUES ($1, 1, $2, 'VACUUM x', 'analyzer', now() - $3::interval)`, id, hash, age)
	execRetry(t, ctx, `INSERT INTO sage.recommendation_transition
		(recommendation_id, to_state, revision, actor, created_at)
		VALUES ($1, 'proposed', 1, 'test', now() - $2::interval)`, id, age)
	return id
}

// Terminal recommendations older than actions_days are purged with their
// revisions and history; every non-terminal row survives however old.
func TestRetentionPurgesOnlyOldTerminalRecommendations(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("rec_retention")
	live := []string{"proposed", "approved", "applying", "applied", "verifying", "failed"}
	terminal := []string{"verified", "reverted", "inconclusive", "superseded", "abandoned"}
	keep := map[int64]string{}
	purge := map[int64]string{}
	for _, state := range live {
		keep[insertRecommendation(t, ctx, tag, state, "400 days")] = state + " (old)"
	}
	for _, state := range terminal {
		purge[insertRecommendation(t, ctx, tag, state, "400 days")] = state + " (old)"
		keep[insertRecommendation(t, ctx, tag, state, "1 hour")] = state + " (recent)"
	}

	New(testPool, allDays(30), noopLog).Run(ctx)

	for id, what := range keep {
		if countWhere(t, ctx, `SELECT count(*) FROM sage.recommendation WHERE id=$1`, id) != 1 {
			t.Errorf("%s recommendation %d was purged", what, id)
		}
		if countWhere(t, ctx, `SELECT count(*) FROM sage.recommendation_revision
			WHERE recommendation_id=$1`, id) != 1 {
			t.Errorf("%s recommendation %d lost its revision", what, id)
		}
	}
	for id, what := range purge {
		n := countWhere(t, ctx, `SELECT
			(SELECT count(*) FROM sage.recommendation WHERE id=$1) +
			(SELECT count(*) FROM sage.recommendation_revision WHERE recommendation_id=$1) +
			(SELECT count(*) FROM sage.recommendation_transition
			  WHERE recommendation_id=$1)`, id)
		if n != 0 {
			t.Errorf("%s recommendation %d: %d rows remain, want 0", what, id, n)
		}
	}
}

func TestRetentionDisabledKeepsRecommendations(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("rec_retention_off")
	id := insertRecommendation(t, ctx, tag, "verified", "400 days")
	New(testPool, allDays(0), noopLog).Run(ctx)
	if countWhere(t, ctx, `SELECT count(*) FROM sage.recommendation WHERE id=$1`, id) != 1 {
		t.Fatal("actions_days=0 purged a recommendation")
	}
}

// An action a live recommendation still points at is its apply evidence;
// the action_log purge must keep it.
func TestRetentionKeepsActionOfLiveRecommendation(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("rec_action")
	actionID := insertID(t, ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, executed_at)
		VALUES ('create_index', $1, 'monitoring', now() - interval '400 days')
		RETURNING id`, tag)
	recID := insertRecommendation(t, ctx, tag, "verifying", "400 days")
	execRetry(t, ctx, `UPDATE sage.recommendation SET action_log_id=$2 WHERE id=$1`,
		recID, actionID)
	New(testPool, allDays(30), noopLog).Run(ctx)
	if countWhere(t, ctx, `SELECT count(*) FROM sage.action_log WHERE id=$1`, actionID) != 1 {
		t.Fatal("the action of a verifying recommendation was purged")
	}
}
