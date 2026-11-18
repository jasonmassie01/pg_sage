package clone_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
)

// lab-05c CHECK-33 (OWNER ACTION, decision D2): the clone contract on a
// real, funded Aurora PostgreSQL cluster. It creates billable clusters
// holding production data, so it runs only with SAGE_AURORA_LIVE=1 and
//
//	SAGE_AURORA_LIVE_DATABASE_URL  the monitored cluster's endpoint DSN
//	SAGE_AURORA_REGION             its region
//	SAGE_TEST_DATABASE_URL         a scratch database for the receipts
//
// with AWS credentials in the default chain. Afterwards
// `aws rds describe-db-clusters` must show no sage-* cluster.
const auroraLiveFlag = "SAGE_AURORA_LIVE"

func TestAuroraLive_ContractOnARealCluster(t *testing.T) {
	contracttest.RequireLive(t, auroraLiveFlag)
	dsn, region := os.Getenv("SAGE_AURORA_LIVE_DATABASE_URL"), os.Getenv("SAGE_AURORA_REGION")
	if dsn == "" || region == "" {
		t.Fatal(auroraLiveFlag + " is set but SAGE_AURORA_LIVE_DATABASE_URL or " +
			"SAGE_AURORA_REGION is not")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prodPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid SAGE_AURORA_LIVE_DATABASE_URL")
	}
	t.Cleanup(prodPool.Close)
	identity, err := clone.PoolIdentity(prodPool)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	api := clone.NewAuroraSDKAPI(rds.NewFromConfig(awsCfg))
	pool := auroraReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		p, err := clone.NewAuroraCloneProvider(clone.AuroraConfig{Region: region,
			TTL: 2 * time.Hour, WaitTimeout: 40 * time.Minute,
			API: func(context.Context) (clone.AuroraAPI, error) { return api, nil },
			Production: func(context.Context) (clone.AuroraProduction, error) {
				return clone.AuroraProduction{DSN: dsn}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.Subject{Adapter: clone.AuroraAdapter, Provider: p,
			Production: identity, Exists: liveAuroraExists(api)}
	}, contracttest.Options{Pool: pool, DeploymentID: auroraDeployment})
}

// liveAuroraExists reports a cluster as live until RDS no longer
// describes it or it is deleting.
func liveAuroraExists(api clone.AuroraAPI) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, ref string) (bool, error) {
		c, err := api.DescribeCluster(ctx, ref)
		if errors.Is(err, clone.ErrAuroraNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return c.Status != "deleting", nil
	}
}
