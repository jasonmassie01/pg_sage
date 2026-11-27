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
//
// obs (nil is safe) records every observation, an empty one included, for
// the migration detector, and redirects the finding of a chain that is
// the queue behind a tracked in-flight DDL (migration-05).
func ProbeLockChains(
	ctx context.Context, pool *pgxpool.Pool, cfg *config.Config,
	obs *LockChainObserver,
) ([]Finding, error) {
	if pool == nil || cfg == nil || !cfg.Analyzer.LockChain.Enabled {
		return nil, nil
	}
	chains, err := DetectLockChains(ctx, pool, cfg)
	if err != nil {
		return nil, err
	}
	obs.Record(chains)
	if len(chains) == 0 {
		return nil, nil
	}
	var ownPID int
	if err := pool.QueryRow(ctx,
		"SELECT pg_backend_pid()").Scan(&ownPID); err != nil {
		return nil, fmt.Errorf("lock chain own backend pid: %w", err)
	}
	findings := lockChainFindings(chains, cfg.Analyzer.LockChain, ownPID)
	redirectQueuedDDL(findings, chains, obs.queuedDDL())
	return findings, nil
}
