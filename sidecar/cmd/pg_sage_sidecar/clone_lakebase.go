package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/lakebaseid"
)

// lakebaseCloneProvider is the lakebase_branch adapter (market-03): one
// instant branch of production per clone. monitored is the production
// database; without it the adapter can destroy but never creates.
func lakebaseCloneProvider(cloneCfg config.CloneProviderConfig,
	monitored *pgxpool.Pool) (clonepkg.Provider, error) {
	lb := cloneCfg.Lakebase
	p, err := clonepkg.NewLakebaseBranchProvider(clonepkg.LakebaseConfig{
		Project: lb.Project, SourceBranch: lb.SourceBranch,
		TTL:           time.Duration(lb.TTLMinutes) * time.Minute,
		EndpointMaxCU: lb.EndpointMaxCU,
		API:           lakebaseBranchAPI,
		Production:    lakebaseProduction(monitored),
	})
	if err != nil {
		// A nil interface, not a typed nil: callers test the adapter for nil.
		return nil, err
	}
	return p, nil
}

// lakebaseBranchAPI is the workspace's Lakebase control plane, through
// Databricks unified auth; its error makes a rehearsal recommend-only.
func lakebaseBranchAPI() (clonepkg.LakebaseBranchAPI, error) {
	pg, err := databricksauth.Postgres()
	if err != nil {
		return nil, fmt.Errorf("configure Databricks unified authentication: %w", err)
	}
	return clonepkg.NewLakebaseSDKAPI(pg), nil
}

// lakebaseProduction reads the identity of the Lakebase endpoint pool
// connects to (SAGE_LAKEBASE_ENDPOINT or SAGE_LAKEBASE_ENDPOINTS), verified
// against live metadata as telemetry scope does. nil for a nil pool.
func lakebaseProduction(
	pool *pgxpool.Pool,
) func(context.Context) (clonepkg.LakebaseProduction, error) {
	if pool == nil {
		return nil
	}
	return func(ctx context.Context) (clonepkg.LakebaseProduction, error) {
		conn := pool.Config().ConnConfig
		endpoint, err := lakebaseid.EndpointForHost(os.Getenv, conn.Host)
		if err != nil {
			return clonepkg.LakebaseProduction{}, err
		}
		if endpoint == "" {
			return clonepkg.LakebaseProduction{}, fmt.Errorf("the monitored database on %s "+
				"is not a Lakebase endpoint: set SAGE_LAKEBASE_ENDPOINT", conn.Host)
		}
		res, err := databricksauth.ResolveTelemetryResource(ctx, conn.Host, endpoint)
		if err != nil {
			return clonepkg.LakebaseProduction{}, err
		}
		project, branch, _, err := lakebaseid.Parse(endpoint)
		if err != nil {
			return clonepkg.LakebaseProduction{}, err
		}
		return clonepkg.LakebaseProduction{
			BranchName: "projects/" + project + "/branches/" + branch,
			BranchUID:  res.BranchID, Host: conn.Host, User: conn.User,
			Database: conn.Database,
		}, nil
	}
}
