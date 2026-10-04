package approvalcard

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/approvalcard"))
}

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// queueOptimizerIndex inserts an open optimizer finding and its queued
// CREATE INDEX proposal, returning the queue id and the finding id.
func queueOptimizerIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (int, int) {
	t.Helper()
	ident := fmt.Sprintf("public.orders|btree(c_%d)", time.Now().UnixNano())
	sql := "CREATE INDEX CONCURRENTLY idx_" + strconv.FormatInt(time.Now().UnixNano(), 36) +
		" ON public.orders (customer_id)"
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql,
		status) VALUES ('index_optimization', 'warning', 'index', $1,
		'Index recommendation for public.orders', $2::jsonb, 'Index customer_id', $3,
		'open') RETURNING id`, ident, `{"table":"public.orders",
		"llm_rationale":"seq scans filter by customer_id","confidence_score":0.8,
		"estimated_improvement_pct":42.5,"estimated_size_bytes":8388608,
		"what_if_verdict":"unverified","affected_queries":["SELECT 1"],"queryids":[77],
		"seq_scan":9000}`, sql).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	id, err := store.NewActionStore(pool).ProposeWithMetadata(ctx, nil, findingID, sql,
		"DROP INDEX CONCURRENTLY public.idx_x", "moderate", store.ActionProposalMetadata{
			ActionType: "create_index_concurrently", PolicyDecision: "queue_approval",
			Guardrails: []string{"approval_required"}})
	if err != nil {
		t.Fatal(err)
	}
	return id, findingID
}
