package schema

import (
	"context"
	"strings"
	"testing"
)

// Sage SRE M7 (AI-SRE-SPEC §7.3, §10 sre_family_autonomy, sre_eval_runs):
// the earned-autonomy ledger tables exist after bootstrap, bootstrap is
// idempotent, levels above L3 (L4 is reserved) are refused by the
// database itself, a promotion moves exactly one level, and the history
// tables cannot be rewritten.
const m7Deployment = "77777777-7777-4777-8777-777777777777"

func TestSREMigrationM7_TablesExistAndBootstrapIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"sre_family_autonomy", "sre_autonomy_proposals",
		"sre_autonomy_events", "sre_autonomy_outcomes", "sre_packet_reviews",
		"sre_eval_runs", "sre_game_days", "rollout_instance"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'sage' AND table_name = $1`, table).Scan(&n); err != nil ||
			n != 1 {
			t.Errorf("sage.%s: %d (%v)", table, n, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'rollout_run'
		  AND column_name IN ('halt_reason', 'family', 'action_class', 'started_by')`).
		Scan(&cols); err != nil || cols != 4 {
		t.Fatalf("rollout_run M7 columns = %d (%v), want 4", cols, err)
	}
}

func m7Cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := requireDB(t)
	clean := func() {
		bg := context.Background()
		for _, table := range []string{"sre_autonomy_proposals", "sre_family_autonomy",
			"sre_packet_reviews"} {
			_, _ = pool.Exec(bg, "DELETE FROM sage."+table+" WHERE deployment_id = $1",
				m7Deployment)
		}
	}
	clean()
	t.Cleanup(clean)
}

func TestSREMigrationM7_LevelBoundsAreEnforced(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	m7Cleanup(t, ctx)
	insert := func(level int) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.sre_family_autonomy
			(deployment_id, family, action_class, level, changed_by, change_reason)
			VALUES ($1, 'lock_blocking', 'backend_cancel', $2, 'test', 'bounds')
			ON CONFLICT (deployment_id, database_name, family, action_class)
			DO UPDATE SET level = EXCLUDED.level`, m7Deployment, level)
		return err
	}
	for _, level := range []int{0, 1, 2, 3} {
		if err := insert(level); err != nil {
			t.Fatalf("level %d refused: %v", level, err)
		}
	}
	for _, level := range []int{4, 5, -1} {
		if err := insert(level); err == nil {
			t.Fatalf("level %d accepted; L4 is reserved and never stored", level)
		}
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT level FROM sage.sre_family_autonomy
		WHERE deployment_id = $1 AND family = 'lock_blocking'
		  AND action_class = 'backend_cancel'`, m7Deployment).Scan(&stored); err != nil ||
		stored != 3 {
		t.Fatalf("stored level = %d (%v), want 3", stored, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, family, action_class, level, changed_by, change_reason)
		VALUES ($1, 'Lock Blocking; drop', 'x', 1, 'test', 'bad name')`,
		m7Deployment); err == nil {
		t.Fatal("malformed family name accepted")
	}
}

func TestSREMigrationM7_ProposalsMoveOneLevelAndOnePendingPerPair(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	m7Cleanup(t, ctx)
	propose := func(id string, from, to int) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_proposals
			(deployment_id, id, family, action_class, from_level, to_level, evidence,
			 evidence_sha256, status, expires_at)
			VALUES ($1, $2, 'wal_retention', 'wal_bound', $3, $4, '{}',
			        sha256('e'::bytea), 'pending', clock_timestamp() + interval '1 day')`,
			m7Deployment, id, from, to)
		return err
	}
	if err := propose("a1111111-1111-4111-8111-111111111111", 1, 3); err == nil {
		t.Fatal("a two-level jump was accepted")
	}
	if err := propose("a2222222-2222-4222-8222-222222222222", 3, 4); err == nil {
		t.Fatal("a proposal to L4 was accepted")
	}
	if err := propose("a3333333-3333-4333-8333-333333333333", 1, 2); err != nil {
		t.Fatalf("valid proposal refused: %v", err)
	}
	err := propose("a4444444-4444-4444-8444-444444444444", 1, 2)
	if err == nil || !strings.Contains(err.Error(), "sre_autonomy_one_pending") {
		t.Fatalf("second pending proposal for one pair: %v", err)
	}
}

func TestSREMigrationM7_HistoryIsAppendOnly(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.sre_autonomy_events
		(deployment_id, family, action_class, event_type, from_level, to_level,
		 actor, reason)
		VALUES ($1, 'lock_blocking', 'backend_cancel', 'downgraded', 2, 1, 'test',
		        'append-only check') RETURNING id`, m7Deployment).Scan(&id); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_autonomy_events SET reason = 'x'
		WHERE id = $1`, id); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("event rewrite: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_events
		(deployment_id, family, action_class, event_type, actor, reason)
		VALUES ($1, 'lock_blocking', 'backend_cancel', 'level_set_by_hand', 't', 'r')`,
		m7Deployment); err == nil {
		t.Fatal("unknown event type accepted")
	}
	var outcome int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.sre_autonomy_outcomes
		(deployment_id, database_name, family, action_class, level, result, source,
		 actor) VALUES ($1, 'db1', 'lock_blocking', 'backend_cancel', 2,
		 'verified_recovery', 'executor', 'pg_sage') RETURNING id`, m7Deployment).
		Scan(&outcome); err != nil {
		t.Fatalf("insert outcome: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_autonomy_outcomes SET result = 'harmful'
		WHERE id = $1`, outcome); err == nil {
		t.Fatal("an outcome was rewritten")
	}
}

func TestSREMigrationM7_OutcomesAreIdempotentPerAction(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	record := func() (int64, error) {
		tag, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_outcomes
			(deployment_id, database_name, action_log_id, family, action_class, level,
			 result, source, actor)
			VALUES ($1, 'db-idem', 4242, 'wraparound_runway', 'freeze', 3,
			        'verified_recovery', 'executor', 'pg_sage')
			ON CONFLICT DO NOTHING`, m7Deployment)
		return tag.RowsAffected(), err
	}
	first, err := record()
	if err != nil || first != 1 {
		t.Fatalf("first record = %d (%v)", first, err)
	}
	second, err := record()
	if err != nil || second != 0 {
		t.Fatalf("repeat record = %d (%v), want a no-op", second, err)
	}
}
