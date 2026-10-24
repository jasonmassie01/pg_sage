package mcp

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/value"
)

// CHECK-RCP-06 over MCP: get_value reports the same Change Review figures
// as GET /api/v1/value (one final review credit, one pending).

func TestGetValueShowsChangeReviewCredit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "mcp_cr_value"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.change_review (database_name, source,
		actor, input_hash, state, stage, verdict, deterministic_verdict, parse_mode,
		model_status, landing_state, receipt_verdict, dba_verdict, dba_actor, credit_state,
		credited_minutes, toil_model_version, credit_final_at, completed_at)
		SELECT 'db', 'mcp', 'a', md5(g::text) || md5(g::text), 'complete', 'done', 'safe',
		'safe', 'ast', 'no_model', CASE WHEN g = 1 THEN 'expired' ELSE 'awaiting' END,
		CASE WHEN g = 1 THEN 'unverifiable' ELSE 'pending' END, 'concur', 'user:1',
		CASE WHEN g = 1 THEN 'final' ELSE 'pending' END, 15, 1,
		CASE WHEN g = 1 THEN now() END, now()
		FROM generate_series(1, 2) g`); err != nil {
		t.Fatalf("seed reviews: %v", err)
	}
	reader := value.NewFleetService(func() []value.Source {
		return []value.Source{{Name: "db", Pool: pool}}
	})
	got, err := NewValueAccess(reader).GetValue(ctx)
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	features, ok := got["by_feature"].(map[string]any)
	if !ok {
		t.Fatalf("by_feature %T", got["by_feature"])
	}
	if v, _ := features["change_review"].(float64); math.Abs(v-0.25) > 1e-9 {
		t.Fatalf("by_feature.change_review = %v, want 0.25", features["change_review"])
	}
	if v, _ := got["potential_hours_pending"].(float64); math.Abs(v-0.25) > 1e-9 {
		t.Fatalf("potential_hours_pending = %v, want 0.25", got["potential_hours_pending"])
	}
}
