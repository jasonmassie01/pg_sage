package optimizer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
)

func connectFindingsDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := connectTestDB(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM sage.findings"); err != nil {
		pool.Close()
		t.Fatalf("reset findings: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func insertOpenFinding(t *testing.T, pool *pgxpool.Pool, category, ident string, detail map[string]any) {
	t.Helper()
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, status)
		VALUES ($1, 'warning', 'index', $2, 't', $3, 'r', $4, 'open')`,
		category, ident, raw, detail["ddl"])
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
}

// C06/G3-B12: an open optimizer finding for the table (stored with the
// emitted identity) suppresses the LLM call and the recommendation is
// re-emitted unchanged so the analyzer does not resolve it.
func TestOpenRecommendations_SkipLLMAndReEmit(t *testing.T) {
	pool := connectFindingsDB(t)
	rec := sampleRecommendation()
	insertOpenFinding(t, pool, "missing_index",
		"public.orders|btree(status)", map[string]any{
			"ddl":      rec.DDL,
			"drop_ddl": `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_orders_status"`,
			"table":    "public.orders", "confidence_score": 0.8,
			"queryids": []int64{1, 2},
		})

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write(fnTestChatJSON("[]", 5))
	}))
	defer srv.Close()
	o := New(llm.New(fnTestLLMConfig(srv.URL), fnNoopLog), nil, pool,
		fnTestOptimizerConfig(), 160000, 8192, fnNoopLog)

	unavailable := false
	o.hypopg.available = &unavailable

	recs, open := o.openRecommendations(context.Background(), sampleTableContext())
	if !open {
		t.Fatal("open finding for public.orders not detected")
	}
	if len(recs) != 1 || recs[0].DDL != rec.DDL {
		t.Fatalf("re-emitted recs = %+v, want the stored DDL", recs)
	}
	if recs[0].FindingIdentifier() != "public.orders|btree(status)" {
		t.Errorf("re-emitted identity = %q", recs[0].FindingIdentifier())
	}
	if calls.Load() != 0 {
		t.Errorf("LLM called %d times", calls.Load())
	}
}

// C06: LIKE wildcards in table names must not match other tables, and
// other categories/tables do not count.
func TestOpenRecommendations_ExactTableMatch(t *testing.T) {
	pool := connectFindingsDB(t)
	insertOpenFinding(t, pool, "missing_index", "public.aXb|btree(x)",
		map[string]any{"ddl": "CREATE INDEX CONCURRENTLY i ON public.aXb (x)"})
	insertOpenFinding(t, pool, "unused_index", "public.a_b.idx",
		map[string]any{"ddl": "DROP INDEX CONCURRENTLY idx"})
	o := New(nil, nil, pool, fnTestOptimizerConfig(), 160000, 8192, fnNoopLog)
	tc := sampleTableContext()
	tc.Table = "a_b"
	if _, open := o.openRecommendations(context.Background(), tc); open {
		t.Error("public.a_b matched a finding for public.aXb or another category")
	}
}
