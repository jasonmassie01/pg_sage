package optimizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// HypoPG measures recommendations with hypothetical indexes.
type HypoPG struct {
	pool  *pgxpool.Pool
	logFn func(string, string, ...any)
	// lockTimeout bounds each lock wait of a what-if session; the
	// optimizer sets the safety configuration's (catalogread Lock).
	lockTimeout time.Duration
	mu          sync.Mutex
	available   *bool
}

// NewHypoPG measures with the shipped safety lock timeout.
func NewHypoPG(pool *pgxpool.Pool, logFn func(string, string, ...any)) *HypoPG {
	return &HypoPG{pool: pool, logFn: logFn, lockTimeout: catalogread.Default().Lock}
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

// Validate measures an index on one owning session and restores that
// session before release. It plans at most maxWhatIfStatements statements
// (whatIfSample: the recommendation's affected queryids, then the hottest
// by total time). Each workload query is planned in its own savepoint: one
// that cannot be planned is counted in Failed instead of aborting the
// evaluation, but a lock timeout ends it with an error. The verdict is the
// caller's (whatIfVerdict).
func (h *HypoPG) Validate(ctx context.Context, rec Recommendation, queries []QueryInfo,
) (res WhatIfResult, retErr error) {
	if h.pool == nil {
		return WhatIfResult{}, fmt.Errorf("HypoPG requires a database pool")
	}
	if err := ctx.Err(); err != nil {
		return WhatIfResult{}, err
	}
	queries = whatIfSample(queries, rec.AffectedQueryIDs)
	if len(queries) == 0 {
		return WhatIfResult{}, nil
	}
	session, err := openHypoPGSession(ctx, h.pool, h.lockTimeout)
	if err != nil {
		return WhatIfResult{}, err
	}
	defer func() {
		if err := session.close(); err != nil {
			res = WhatIfResult{}
			retErr = errors.Join(retErr, err)
		}
	}()
	res, err = session.evaluate(ctx, rec.DDL, rec.Alongside, queries)
	if err != nil {
		return WhatIfResult{}, err
	}
	return res, nil
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
