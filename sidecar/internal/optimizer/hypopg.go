package optimizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/workload"
)

// hypopgRecheck is how long a probe that found HypoPG absent stands: an
// operator (or a provider) may install it while pg_sage runs.
const hypopgRecheck = 10 * time.Minute

// hypopgInstalledSQL is the availability probe (a catalog read).
const hypopgInstalledSQL = `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_extension
	WHERE extname='hypopg')`

// HypoPG measures recommendations with hypothetical indexes.
type HypoPG struct {
	pool  *pgxpool.Pool
	logFn func(string, string, ...any)
	// lockTimeout bounds each lock wait of a what-if session; the
	// optimizer sets the safety configuration's (catalogread Lock).
	lockTimeout time.Duration
	// readTimeouts bound the availability probe (a catalogread read).
	readTimeouts catalogread.Timeouts
	now          func() time.Time
	probe        func(context.Context) (bool, error) // nil: probeInstalled
	mu           sync.Mutex
	available    *bool     // the last successful probe's answer
	checkedAt    time.Time // when available was probed
}

// NewHypoPG measures with the shipped safety lock timeout.
func NewHypoPG(pool *pgxpool.Pool, logFn func(string, string, ...any)) *HypoPG {
	return &HypoPG{pool: pool, logFn: logFn, lockTimeout: catalogread.Default().Lock,
		readTimeouts: catalogread.Default(), now: time.Now}
}

// IsAvailable reports whether HypoPG is installed. A found extension is
// cached; an absent one is re-probed hypopgRecheck after its probe; a
// failed probe is logged and never cached. Callers serialize on the probe,
// which is a bounded catalog read.
func (h *HypoPG) IsAvailable(ctx context.Context) bool {
	if h.pool == nil && h.probe == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.available != nil && (*h.available || h.now().Sub(h.checkedAt) < hypopgRecheck) {
		return *h.available
	}
	probe := h.probe
	if probe == nil {
		probe = h.probeInstalled
	}
	installed, err := probe(ctx)
	if err != nil {
		h.logFn("WARN", "optimizer: HypoPG availability probe failed, treating HypoPG "+
			"as unavailable until the next check: %v", err)
		return false
	}
	h.available, h.checkedAt = &installed, h.now()
	return installed
}

func (h *HypoPG) probeInstalled(ctx context.Context) (bool, error) {
	var installed bool
	err := catalogread.New(h.pool, h.readTimeouts).QueryRow(ctx, hypopgInstalledSQL).
		Scan(&installed)
	return installed, err
}

// Validate measures an index on one owning session and restores that
// session before release. It plans at most maxWhatIfStatements of the
// explainable statements (whatIfSample: the recommendation's affected
// queryids, then the hottest by total time). Each workload query is
// planned in its own savepoint: one that cannot be planned is counted in
// Failed instead of aborting the evaluation, but a lock timeout ends it
// with an error. The verdict is the caller's (whatIfVerdict).
func (h *HypoPG) Validate(ctx context.Context, rec Recommendation, queries []QueryInfo,
) (WhatIfResult, error) {
	if h.pool == nil {
		return WhatIfResult{}, fmt.Errorf("HypoPG requires a database pool")
	}
	if err := ctx.Err(); err != nil {
		return WhatIfResult{}, err
	}
	queries = whatIfSample(explainableQueries(queries), rec.AffectedQueryIDs)
	if len(queries) == 0 {
		return WhatIfResult{}, nil
	}
	return h.inSession(ctx, func(s *hypopgSession) (WhatIfResult, error) {
		return s.evaluate(ctx, rec, queries)
	})
}

// inSession runs measure on one owning session and restores the session
// before release. A failed measurement or cleanup leaves no evidence.
func (h *HypoPG) inSession(ctx context.Context,
	measure func(*hypopgSession) (WhatIfResult, error),
) (res WhatIfResult, retErr error) {
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
	res, err = measure(session)
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

// explainableQueries are the statements the what-if can plan: the
// read-only ones workload.Plannable accepts. A table's writes are never
// planned, so they take none of the what-if's statement slots. queries
// is not modified.
func explainableQueries(queries []QueryInfo) []QueryInfo {
	var out []QueryInfo
	for _, q := range queries {
		if ok, readOnly := workload.Plannable(q.Text); ok && readOnly {
			out = append(out, q)
		}
	}
	return out
}
