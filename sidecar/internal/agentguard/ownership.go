package agentguard

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentposture"
)

// apOwnership is the posture detector for objects owned by agent roles.
const apOwnership = "AP-02"

// AgentOwnership runs the AP-02 posture detector on pool's database (and
// the cluster's shared catalogs) and returns its findings about registered
// agent roles; client-name hints are not agent roles and are left out.
// G1-07 requires none after any Guard action.
func AgentOwnership(ctx context.Context, pool *pgxpool.Pool) ([]agentposture.Finding, error) {
	if pool == nil {
		return nil, invalid("no database pool for the ownership check")
	}
	det, ok := agentposture.Default().Get(apOwnership)
	if !ok {
		return nil, fmt.Errorf("agentguard: posture detector %s is not registered", apOwnership)
	}
	cfg := agentposture.DefaultConfig()
	cfg.ClientPatterns = nil // registered roles only
	var out []agentposture.Finding
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			env, err := agentposture.ResolveEnv(ctx, tx, cfg)
			if err != nil {
				return err
			}
			res, err := agentposture.RunDetector(ctx, det, tx, env)
			out = res.Findings
			return err
		})
	if err != nil {
		return nil, fmt.Errorf("agentguard: ownership check: %w", err)
	}
	return out, nil
}

// VerifyNoAgentOwnership is AgentOwnership as a check: ErrPostCheck naming
// the owning roles when any agent role owns an object (G1-07).
func VerifyNoAgentOwnership(ctx context.Context, pool *pgxpool.Pool) error {
	found, err := AgentOwnership(ctx, pool)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return nil
	}
	names := make([]string, len(found))
	for i, f := range found {
		names[i] = f.Title
	}
	return fmt.Errorf("%w: %v", ErrPostCheck, names)
}
