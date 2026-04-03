//go:build e2e

// Package e2e — tests that the LLM-powered index optimizer can
// consolidate 8 different query patterns for one table into a
// minimal set of indexes (ideally fewer than 8).
//
// Run with:
//
//	SAGE_LLM_API_KEY=<key> go test -tags=e2e -count=1 \
//	    -timeout 300s ./e2e/ -run TestOptimizerMultiQuery
package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// TestOptimizerMultiQueryConsolidation sends 8 distinct query
// patterns for a single table to the optimizer prompt and
// verifies the LLM recommends fewer than 8 indexes — proving
// it consolidates coverage across queries.
func TestOptimizerMultiQueryConsolidation(t *testing.T) {
	apiKey := requireAPIKey(t)
	cfg := newTestLLMConfig(apiKey, largeBudget)
	cfg.TimeoutSeconds = 90 // large prompt needs more time
	client := llm.New(cfg, testLogFn(t))

	tc := optimizer.TableContext{
		Schema:     "public",
		Table:      "orders",
		LiveTuples: 15_000_000,
		DeadTuples: 50_000,
		TableBytes: 2_500_000_000,
		IndexBytes: 900_000_000,
		WriteRate:  18.5,
		IndexCount: 2,
		Workload:   "oltp_read",
		Collation:  "en_US.UTF-8",
		Columns: []optimizer.ColumnInfo{
			{Name: "id", Type: "bigint"},
			{Name: "customer_id", Type: "bigint"},
			{Name: "status", Type: "text"},
			{Name: "created_at", Type: "timestamptz"},
			{Name: "updated_at", Type: "timestamptz"},
			{Name: "total_amount", Type: "numeric(12,2)"},
			{Name: "region", Type: "text"},
			{Name: "shipping_method", Type: "text"},
		},
		ColStats: []optimizer.ColStat{
			{Column: "id", NDistinct: -1.0, Correlation: 0.99},
			{Column: "customer_id", NDistinct: -0.02, Correlation: 0.15},
			{Column: "status", NDistinct: 5, Correlation: 0.10},
			{Column: "created_at", NDistinct: -0.85, Correlation: 0.98},
			{Column: "region", NDistinct: 12, Correlation: 0.05},
		},
		Indexes: []optimizer.IndexInfo{
			{
				Name:       "orders_pkey",
				Definition: "btree (id)",
				Scans:      9_500_000,
				IsUnique:   true,
			},
			{
				Name:       "idx_orders_created",
				Definition: "btree (created_at)",
				Scans:      120_000,
			},
		},
		// 8 distinct query patterns that share columns:
		// customer_id, status, created_at, region
		Queries: []optimizer.QueryInfo{
			{
				// Q1: lookup by customer_id, sort by created_at
				QueryID: 1, Calls: 450_000,
				MeanTimeMs: 12.5, TotalTimeMs: 5_625_000,
				Text: "SELECT * FROM orders " +
					"WHERE customer_id = $1 " +
					"ORDER BY created_at DESC LIMIT 50",
			},
			{
				// Q2: filter by status + date range
				QueryID: 2, Calls: 85_000,
				MeanTimeMs: 45.2, TotalTimeMs: 3_842_000,
				Text: "SELECT * FROM orders " +
					"WHERE status = $1 " +
					"AND created_at > $2",
			},
			{
				// Q3: aggregate by customer in date range
				QueryID: 3, Calls: 12_000,
				MeanTimeMs: 120.0, TotalTimeMs: 1_440_000,
				Text: "SELECT customer_id, " +
					"SUM(total_amount) " +
					"FROM orders " +
					"WHERE created_at > $1 " +
					"GROUP BY customer_id",
			},
			{
				// Q4: customer + status combo
				QueryID: 4, Calls: 200_000,
				MeanTimeMs: 8.0, TotalTimeMs: 1_600_000,
				Text: "SELECT id, status, total_amount " +
					"FROM orders " +
					"WHERE customer_id = $1 " +
					"AND status = $2",
			},
			{
				// Q5: region report in date range
				QueryID: 5, Calls: 5_000,
				MeanTimeMs: 250.0, TotalTimeMs: 1_250_000,
				Text: "SELECT region, COUNT(*), " +
					"SUM(total_amount) " +
					"FROM orders " +
					"WHERE created_at BETWEEN $1 AND $2 " +
					"GROUP BY region",
			},
			{
				// Q6: recent orders by customer, specific status
				QueryID: 6, Calls: 95_000,
				MeanTimeMs: 15.0, TotalTimeMs: 1_425_000,
				Text: "SELECT * FROM orders " +
					"WHERE customer_id = $1 " +
					"AND status = 'active' " +
					"AND created_at > $2 " +
					"ORDER BY created_at DESC",
			},
			{
				// Q7: existence check by customer
				QueryID: 7, Calls: 300_000,
				MeanTimeMs: 3.0, TotalTimeMs: 900_000,
				Text: "SELECT 1 FROM orders " +
					"WHERE customer_id = $1 LIMIT 1",
			},
			{
				// Q8: status dashboard count
				QueryID: 8, Calls: 20_000,
				MeanTimeMs: 85.0, TotalTimeMs: 1_700_000,
				Text: "SELECT status, COUNT(*) " +
					"FROM orders " +
					"WHERE created_at > $1 " +
					"GROUP BY status",
			},
		},
	}

	system := optimizer.SystemPrompt()
	user := optimizer.FormatPrompt(tc)

	ctx, cancel := context.WithTimeout(
		context.Background(), 120*time.Second,
	)
	defer cancel()

	resp, tokens, err := client.Chat(
		ctx, system, user, 4096,
	)
	if err != nil {
		t.Fatalf("optimizer chat failed: %v", err)
	}

	t.Logf("Response (%d tokens):\n%s", tokens, resp)

	cleaned := extractJSONArray(resp)
	if cleaned == "" {
		t.Fatalf(
			"could not extract JSON array from: %s", resp,
		)
	}

	var recs []optimizer.Recommendation
	if err := json.Unmarshal(
		[]byte(cleaned), &recs,
	); err != nil {
		t.Fatalf("JSON parse failed: %v\n%s", err, cleaned)
	}

	t.Logf("Got %d index recommendations", len(recs))

	// --- Assertions ---

	if len(recs) == 0 {
		t.Fatal("expected at least 1 recommendation")
	}

	// Core assertion: LLM should consolidate 8 queries into
	// fewer than 8 indexes. A smart optimizer should produce
	// 3-5 well-designed indexes for this workload.
	if len(recs) >= 8 {
		t.Errorf(
			"expected fewer than 8 indexes for 8 queries "+
				"(consolidation), got %d", len(recs),
		)
	}

	// Even stricter: a good optimizer should produce <=5.
	if len(recs) > 5 {
		t.Logf(
			"WARNING: %d indexes for 8 queries — "+
				"suboptimal consolidation", len(recs),
		)
	}

	// Verify customer_id is covered — it appears in Q1, Q4,
	// Q6, Q7 (4 of 8 queries). At least one index must
	// include it.
	customerCovered := false
	for _, r := range recs {
		ddl := strings.ToLower(r.DDL)
		if strings.Contains(ddl, "customer_id") {
			customerCovered = true
			break
		}
	}
	if !customerCovered {
		t.Error(
			"no index covers customer_id, which appears " +
				"in 4 of 8 queries",
		)
	}

	// Verify status is covered — appears in Q2, Q4, Q6, Q8.
	statusCovered := false
	for _, r := range recs {
		ddl := strings.ToLower(r.DDL)
		if strings.Contains(ddl, "status") {
			statusCovered = true
			break
		}
	}
	if !statusCovered {
		t.Error(
			"no index covers status, which appears " +
				"in 4 of 8 queries",
		)
	}

	// Every recommendation must have valid DDL.
	for i, r := range recs {
		upper := strings.ToUpper(r.DDL)
		if !strings.Contains(upper, "CREATE INDEX") {
			t.Errorf(
				"rec[%d]: DDL missing CREATE INDEX: %s",
				i, r.DDL,
			)
		}
		if r.Rationale == "" {
			t.Errorf("rec[%d]: empty rationale", i)
		}
	}

	// Check for composite indexes — at least one should be
	// composite (multi-column) to demonstrate consolidation.
	hasComposite := false
	for _, r := range recs {
		ddl := strings.ToLower(r.DDL)
		// Count commas inside the column list to detect
		// multi-column indexes.
		lparen := strings.LastIndex(ddl, "(")
		if lparen >= 0 {
			colPart := ddl[lparen:]
			if strings.Count(colPart, ",") >= 1 {
				hasComposite = true
				break
			}
		}
	}
	if !hasComposite {
		t.Error(
			"expected at least one composite (multi-column) " +
				"index for query consolidation",
		)
	}

	// Log summary for human review.
	t.Logf("--- Consolidation Summary ---")
	t.Logf("Queries: 8, Indexes recommended: %d", len(recs))
	for i, r := range recs {
		affected := "n/a"
		if len(r.AffectedQueries) > 0 {
			affected = strings.Join(r.AffectedQueries, "; ")
		}
		t.Logf(
			"  [%d] %s\n       type=%s category=%s "+
				"improvement=%.0f%%\n       "+
				"affected_queries: %s\n       "+
				"rationale: %s",
			i+1, r.DDL, r.IndexType, r.Category,
			r.EstimatedImprovementPct, affected,
			r.Rationale,
		)
	}
}
