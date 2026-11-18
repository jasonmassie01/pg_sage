package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/jackc/pgx/v5/pgxpool"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/egress"
)

// awsCredentialProbe bounds the credential check before each clone call.
const awsCredentialProbe = 30 * time.Second

// auroraCloneProvider is the aurora_clone adapter (lab-05c): one Aurora
// copy-on-write clone of the monitored cluster per clone, through the AWS
// default credential chain. monitored is the production database; without
// it the adapter can destroy but never creates.
func auroraCloneProvider(cloneCfg config.CloneProviderConfig,
	monitored *pgxpool.Pool) (clonepkg.Provider, error) {
	a := cloneCfg.Aurora
	p, err := clonepkg.NewAuroraCloneProvider(clonepkg.AuroraConfig{
		Region: a.Region, InstanceClass: a.InstanceClass,
		TTL:        time.Duration(a.TTLMinutes) * time.Minute,
		API:        auroraRDSAPI(a.Region, loadAWSConfig),
		Production: auroraProduction(monitored),
	})
	if err != nil {
		// A nil interface, not a typed nil: callers test the adapter for nil.
		return nil, err
	}
	return p, nil
}

// loadAWSConfig is the AWS default chain for region, as cloud telemetry
// loads it (the credential providers keep their own clients, so an
// instance role is reachable).
func loadAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
}

// auroraRDSAPI is the RDS API of region, on the egress floor's client,
// whose credentials were retrieved (bounded) first, so missing credentials
// make a rehearsal recommend-only before anything is asked of RDS.
func auroraRDSAPI(region string, load func(context.Context, string) (aws.Config, error),
) func(context.Context) (clonepkg.AuroraAPI, error) {
	return func(ctx context.Context) (clonepkg.AuroraAPI, error) {
		client, err := cloneRDSClient(ctx, region, load)
		if err != nil {
			return nil, err
		}
		return clonepkg.NewAuroraSDKAPI(client), nil
	}
}

// cloneRDSClient is the RDS client of region on the egress floor's HTTP
// client, once the AWS default chain's credentials were retrieved
// (bounded): aurora_clone and rds_restore share it.
func cloneRDSClient(ctx context.Context, region string,
	load func(context.Context, string) (aws.Config, error)) (*rds.Client, error) {
	cfg, err := load(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("load the AWS configuration for %s: %w", region, err)
	}
	if cfg.Credentials == nil {
		return nil, errors.New("the AWS default chain has no credential provider")
	}
	probe, cancel := context.WithTimeout(ctx, awsCredentialProbe)
	defer cancel()
	if _, err := cfg.Credentials.Retrieve(probe); err != nil {
		return nil, fmt.Errorf("retrieve AWS credentials: %w", err)
	}
	return rds.NewFromConfig(cfg, func(o *rds.Options) {
		o.HTTPClient = egress.HTTPClient(0)
	}), nil
}

// auroraProduction is the DSN pool was opened with: the monitored cluster
// endpoint and the users a clone reuses. nil for a nil pool.
func auroraProduction(
	pool *pgxpool.Pool,
) func(context.Context) (clonepkg.AuroraProduction, error) {
	if pool == nil {
		return nil
	}
	return func(context.Context) (clonepkg.AuroraProduction, error) {
		return clonepkg.AuroraProduction{DSN: pool.Config().ConnString()}, nil
	}
}
