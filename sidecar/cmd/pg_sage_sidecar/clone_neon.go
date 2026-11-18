package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
)

// neonCloneProvider is the neon_branch adapter (lab-05b): one Neon branch
// of production per clone. monitored is the production database; without
// it the adapter can destroy but never creates.
func neonCloneProvider(cloneCfg config.CloneProviderConfig,
	monitored *pgxpool.Pool) (clonepkg.Provider, error) {
	n := cloneCfg.Neon
	p, err := clonepkg.NewNeonBranchProvider(clonepkg.NeonConfig{
		ProjectID: n.ProjectID, ParentBranch: n.ParentBranch, APIKey: n.APIKey,
		EndpointMaxCU: n.EndpointMaxCU,
		TTL:           time.Duration(n.TTLMinutes) * time.Minute,
		Production:    neonProduction(monitored),
	})
	if err != nil {
		// A nil interface, not a typed nil: callers test the adapter for nil.
		return nil, err
	}
	return p, nil
}

// neonProduction is the identity of the Neon endpoint pool connects to:
// its host (a pooler host counts as its endpoint's), role and database.
// nil for a nil pool.
func neonProduction(
	pool *pgxpool.Pool,
) func(context.Context) (clonepkg.NeonProduction, error) {
	if pool == nil {
		return nil
	}
	return func(context.Context) (clonepkg.NeonProduction, error) {
		conn := pool.Config().ConnConfig
		return clonepkg.NeonProduction{Host: conn.Host, User: conn.User,
			Database: conn.Database}, nil
	}
}
