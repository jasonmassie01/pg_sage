package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// HypoPG validates recommendations using hypothetical indexes.
type HypoPG struct {
	pool              *pgxpool.Pool
	minImprovementPct float64
	logFn             func(string, string, ...any)
	available         *bool // cached availability check
}

// NewHypoPG creates a HypoPG validator.
func NewHypoPG(
	pool *pgxpool.Pool,
	minImprovementPct float64,
	logFn func(string, string, ...any),
) *HypoPG {
	return &HypoPG{
		pool:              pool,
		minImprovementPct: minImprovementPct,
		logFn:             logFn,
	}
}

// IsAvailable returns true if HypoPG extension is installed.
func (h *HypoPG) IsAvailable(ctx context.Context) bool {
	if h.available != nil {
		return *h.available
	}
	var exists bool
	err := h.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'hypopg')",
	).Scan(&exists)
	if err != nil {
		exists = false
	}
	h.available = &exists
	return exists
}

// Validate creates a hypothetical index, runs EXPLAIN, compares costs.
// Returns (accepted, avgImprovement, estimatedSizeBytes, error).
func (h *HypoPG) Validate(
	ctx context.Context,
	rec Recommendation,
	queries []QueryInfo,
) (bool, float64, int64, error) {
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		return false, 0, 0, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	// Measure cost WITHOUT hypothetical index first.
	costsBefore := h.measureCosts(ctx, conn, queries)

	// Create hypothetical index.
	// HypoPG returns indexrelid as oid (PostgreSQL OID 26);
	// pgx binary protocol requires uint32, not int64.
	var indexOID uint32
	err = conn.QueryRow(ctx,
		"SELECT indexrelid FROM hypopg_create_index($1)",
		rec.DDL,
	).Scan(&indexOID)
	if err != nil {
		return false, 0, 0, fmt.Errorf("hypopg_create_index: %w", err)
	}
	// Measure cost WITH hypothetical index.
	costsAfter := h.measureCosts(ctx, conn, queries)

	// Compute average improvement.
	var totalImprovement float64
	var measured int
	for qid, before := range costsBefore {
		after, ok := costsAfter[qid]
		if !ok || before <= 0 {
			continue
		}
		improvement := (before - after) / before * 100
		totalImprovement += improvement
		measured++
	}

	if measured == 0 {
		_, _ = conn.Exec(ctx, "SELECT hypopg_reset()")
		return false, 0, 0, nil
	}

	avgImprovement := totalImprovement / float64(measured)
	accepted := avgImprovement >= h.minImprovementPct

	// Estimate size before cleanup.
	estimatedSize, sizeErr := h.EstimateSize(ctx, int64(indexOID))
	if sizeErr != nil {
		estimatedSize = 0
	}

	// Cleanup hypothetical indexes.
	_, _ = conn.Exec(ctx, "SELECT hypopg_reset()")

	return accepted, avgImprovement, estimatedSize, nil
}

// EstimateSize returns the estimated size of a hypothetical index.
func (h *HypoPG) EstimateSize(
	ctx context.Context,
	indexOID int64,
) (int64, error) {
	if h.pool == nil {
		return 0, fmt.Errorf("pool is nil")
	}
	var size int64
	err := h.pool.QueryRow(ctx,
		"SELECT hypopg_relation_size($1)", indexOID,
	).Scan(&size)
	if err != nil {
		return 0, err
	}
	return size, nil
}

func (h *HypoPG) measureCosts(
	ctx context.Context,
	conn *pgxpool.Conn,
	queries []QueryInfo,
) map[int64]float64 {
	costs := make(map[int64]float64)
	for _, q := range queries {
		if !isExplainable(q.Text) {
			continue
		}
		if err := sanitize.RejectMultiStatement(q.Text); err != nil {
			continue
		}
		_, _ = conn.Exec(ctx,
			"SET SESSION statement_timeout = '5s'")
		var planJSON []byte
		err := conn.QueryRow(ctx,
			"EXPLAIN (FORMAT JSON) "+q.Text,
		).Scan(&planJSON)
		if err != nil {
			continue
		}
		cost := extractTotalCost(planJSON)
		if cost > 0 {
			costs[q.QueryID] = cost
		}
	}
	return costs
}

func extractTotalCost(planJSON []byte) float64 {
	var wrapper []struct {
		Plan struct {
			TotalCost float64 `json:"Total Cost"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &wrapper); err != nil || len(wrapper) == 0 {
		return 0
	}
	return wrapper[0].Plan.TotalCost
}

func isExplainable(query string) bool {
	upper := strings.TrimSpace(strings.ToUpper(query))
	return strings.HasPrefix(upper, "SELECT") ||
		strings.HasPrefix(upper, "WITH")
}
