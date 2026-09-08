package optimizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HypoPG validates recommendations using hypothetical indexes.
type HypoPG struct {
	pool              *pgxpool.Pool
	minImprovementPct float64
	logFn             func(string, string, ...any)
	mu                sync.Mutex
	available         *bool
}

func NewHypoPG(pool *pgxpool.Pool, minImprovementPct float64,
	logFn func(string, string, ...any),
) *HypoPG {
	return &HypoPG{pool: pool, minImprovementPct: minImprovementPct, logFn: logFn}
}

// IsAvailable returns true if HypoPG is installed. Failed probes are not cached.
func (h *HypoPG) IsAvailable(ctx context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.available != nil {
		return *h.available
	}
	if h.pool == nil {
		return false
	}
	var exists bool
	err := h.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_extension WHERE extname='hypopg')",
	).Scan(&exists)
	if err != nil {
		return false
	}
	h.available = &exists
	return exists
}

// Validate measures an index on one owning session and restores that session before release.
func (h *HypoPG) Validate(ctx context.Context, rec Recommendation, queries []QueryInfo,
) (accepted bool, improvement float64, size int64, retErr error) {
	if h.pool == nil {
		return false, 0, 0, fmt.Errorf("HypoPG requires a database pool")
	}
	if err := ctx.Err(); err != nil {
		return false, 0, 0, err
	}
	if len(queries) == 0 {
		return false, 0, 0, nil
	}
	session, err := openHypoPGSession(ctx, h.pool)
	if err != nil {
		return false, 0, 0, err
	}
	defer func() {
		if err := session.close(); err != nil {
			accepted, improvement, size = false, 0, 0
			retErr = errors.Join(retErr, err)
		}
	}()
	improvement, size, err = session.evaluate(ctx, rec.DDL, queries)
	if err != nil {
		return false, 0, 0, err
	}
	return size > 0 && improvement >= h.minImprovementPct, improvement, size, nil
}

func hypotheticalImprovement(before, after map[int64]float64) (float64, int) {
	var total float64
	var measured int
	for id, cost := range before {
		if next, ok := after[id]; ok && cost > 0 {
			total += (cost - next) / cost * 100
			measured++
		}
	}
	if measured == 0 {
		return 0, 0
	}
	return total / float64(measured), measured
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
	return strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH")
}
