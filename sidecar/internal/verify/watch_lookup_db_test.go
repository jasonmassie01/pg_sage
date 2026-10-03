package verify

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Performance gate work queue item 9 (latent): a watch is looked up by
// `baseline->>'watch_id'`, which no index covered, so every lookup read
// every verification ever kept (365 days of executed actions). It must
// be an index lookup.
func TestWatchLookupIsAnIndexLookup(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	cfg := pool.Config().Copy()
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	if _, err := NewPostgresStateStore(traced, 30).Get(ctx, "no-such-watch"); err == nil {
		t.Fatal("a missing watch was found")
	}
	stmts := rec.Matching("sage.verification", "watch_id")
	if len(stmts) != 1 {
		t.Fatalf("lookup statements = %d, want 1", len(stmts))
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `WITH d AS (
		  INSERT INTO sage.decision (feature, intent, target_objects, verdict, risk_tier,
		    reason, evidence, evidence_id)
		  SELECT 'executor', 'create_index', '[]', 'execute', 'safe', 'perf', '{}',
		         'watch-perf-' || g
		  FROM generate_series(1, 6000) g RETURNING id)
		INSERT INTO sage.verification (decision_id, criterion, baseline, minimum_samples,
		  next_evaluation_at, hard_deadline_at, verdict)
		SELECT id, '{}', jsonb_build_object('watch_id', 'perf-watch-' || id), 3, now(),
		       now() + interval '1 hour', 'success'
		FROM d;
		ANALYZE sage.verification`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	plan, err := testdb.Explain(ctx, tx, "", stmts[0].SQL, stmts[0].Args...)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SeqScans("verification") != 0 {
		t.Fatalf("watch lookup scans sage.verification:\n%s", plan)
	}
}
