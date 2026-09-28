package analyzer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// ProbeLockChains runs one lock-chain check and returns the same
// lock_chain findings the analyzer cycle produces, including the root
// blocker's backend identity. It is cheap enough for the RCA fast path
// (Sage SRE M0), which calls it between analyzer cycles. It returns nil
// when lock-chain detection is disabled.
func ProbeLockChains(
	ctx context.Context, pool *pgxpool.Pool, cfg *config.Config,
) ([]Finding, error) {
	if pool == nil || cfg == nil || !cfg.Analyzer.LockChain.Enabled {
		return nil, nil
	}
	chains, err := DetectLockChains(ctx, pool, cfg)
	if err != nil || len(chains) == 0 {
		return nil, err
	}
	var ownPID int
	if err := pool.QueryRow(ctx,
		"SELECT pg_backend_pid()").Scan(&ownPID); err != nil {
		return nil, fmt.Errorf("lock chain own backend pid: %w", err)
	}
	return lockChainFindings(chains, cfg.Analyzer.LockChain, ownPID), nil
}
