package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
)

// alloyDBRestoreProvider is the alloydb_restore adapter (lab-05f): one
// point-in-time restore of the monitored AlloyDB cluster per rehearsal, a
// full copy, through the Google Application Default Credentials chain
// (cloudSQLToken, the token source cloud telemetry uses). monitored is the
// production database; without it the adapter can destroy but never
// creates. A clone lives at most the rehearsal receipt's TTL, which is
// labelled on it.
func alloyDBRestoreProvider(cloneCfg config.CloneProviderConfig,
	monitored *pgxpool.Pool) (clonepkg.Provider, error) {
	s := cloneCfg.AlloyDB
	p, err := clonepkg.NewAlloyDBRestoreProvider(clonepkg.AlloyDBRestoreConfig{
		Project: s.Project, Location: s.Location, Cluster: s.Cluster,
		Deadline: restoreDeadline(cloneCfg), TTL: rehearsalReceiptTTL(cloneCfg),
		Token: cloudSQLToken, Production: alloyDBProduction(monitored),
	})
	if err != nil {
		// A nil interface, not a typed nil: callers test the adapter for nil.
		return nil, err
	}
	return p, nil
}

// alloyDBProduction is the DSN pool was opened with: the monitored
// primary instance's IP and the users a clone reuses. nil for a nil pool.
func alloyDBProduction(
	pool *pgxpool.Pool,
) func(context.Context) (clonepkg.AlloyDBProduction, error) {
	if pool == nil {
		return nil
	}
	return func(context.Context) (clonepkg.AlloyDBProduction, error) {
		return clonepkg.AlloyDBProduction{DSN: pool.Config().ConnString()}, nil
	}
}
