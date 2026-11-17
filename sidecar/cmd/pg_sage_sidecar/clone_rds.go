package main

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5/pgxpool"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
)

// rdsRestoreCloneProvider is the rds_restore adapter (lab-05d): one
// point-in-time restore of the monitored RDS instance per clone, a full
// copy, through the AWS default credential chain. monitored is the
// production database; without it the adapter can destroy but never
// creates. The restored instance lives at most the rehearsal receipt's
// TTL (restore deadline, rehearsal bound and margin), which is tagged on
// it.
func rdsRestoreCloneProvider(cloneCfg config.CloneProviderConfig,
	monitored *pgxpool.Pool) (clonepkg.Provider, error) {
	r := cloneCfg.RDS
	p, err := clonepkg.NewRDSRestoreProvider(clonepkg.RDSRestoreConfig{
		Region: r.Region, InstanceClass: r.InstanceClass,
		Deadline: restoreDeadline(cloneCfg), TTL: rehearsalReceiptTTL(cloneCfg),
		API:        rdsRestoreAPI(r.Region, loadAWSConfig),
		Production: rdsProduction(monitored),
	})
	if err != nil {
		// A nil interface, not a typed nil: callers test the adapter for nil.
		return nil, err
	}
	return p, nil
}

// restoreDeadline is clone.restore_deadline_minutes.
func restoreDeadline(cloneCfg config.CloneProviderConfig) time.Duration {
	return time.Duration(cloneCfg.RestoreDeadlineMinutes) * time.Minute
}

// rdsRestoreAPI is the RDS API of region, on the egress floor's client,
// whose credentials were retrieved (bounded) first, so missing credentials
// make a rehearsal recommend-only before anything is asked of RDS.
func rdsRestoreAPI(region string, load func(context.Context, string) (aws.Config, error),
) func(context.Context) (clonepkg.RDSRestoreClient, error) {
	return func(ctx context.Context) (clonepkg.RDSRestoreClient, error) {
		client, err := cloneRDSClient(ctx, region, load)
		if err != nil {
			return nil, err
		}
		return clonepkg.NewRDSRestoreSDKAPI(client), nil
	}
}

// rdsProduction is the DSN pool was opened with: the monitored instance
// endpoint and the users a restore reuses. nil for a nil pool.
func rdsProduction(
	pool *pgxpool.Pool,
) func(context.Context) (clonepkg.RDSProduction, error) {
	if pool == nil {
		return nil
	}
	return func(context.Context) (clonepkg.RDSProduction, error) {
		return clonepkg.RDSProduction{DSN: pool.Config().ConnString()}, nil
	}
}
