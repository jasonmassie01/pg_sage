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

// lab-05d CHECK-35 (OWNER ACTION, decision D2): the clone contract on a
// real, funded RDS for PostgreSQL instance. Each check restores a whole
// billable instance holding production data, so it runs only with
// SAGE_RDS_LIVE=1 and
//
//	SAGE_RDS_LIVE_DATABASE_URL  the monitored instance's endpoint DSN
//	SAGE_RDS_REGION             its region
//	SAGE_TEST_DATABASE_URL      a scratch database for the receipts
//
// with AWS credentials in the default chain. Afterwards
// `aws rds describe-db-instances` must show no sage-* instance.
const rdsLiveFlag = "SAGE_RDS_LIVE"

func TestRDSRestoreLive_ContractOnARealInstance(t *testing.T) {
	contracttest.RequireLive(t, rdsLiveFlag)
	dsn, region := os.Getenv("SAGE_RDS_LIVE_DATABASE_URL"), os.Getenv("SAGE_RDS_REGION")
	if dsn == "" || region == "" {
		t.Fatal(rdsLiveFlag + " is set but SAGE_RDS_LIVE_DATABASE_URL or SAGE_RDS_REGION " +
			"is not")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prodPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid SAGE_RDS_LIVE_DATABASE_URL")
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
	api := clone.NewRDSRestoreSDKAPI(rds.NewFromConfig(awsCfg))
	pool := rdsReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		p, err := clone.NewRDSRestoreProvider(clone.RDSRestoreConfig{Region: region,
			Deadline: 2 * time.Hour, TTL: 4 * time.Hour,
			API: func(context.Context) (clone.RDSRestoreClient, error) { return api, nil },
			Production: func(context.Context) (clone.RDSProduction, error) {
				return clone.RDSProduction{DSN: dsn}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.Subject{Adapter: clone.RDSRestoreAdapter, Provider: p,
			Production: identity, Exists: liveRDSExists(api)}
	}, contracttest.Options{Pool: pool, DeploymentID: rdsDeployment})
}

// liveRDSExists reports an instance as live until RDS no longer describes
// it or it is deleting.
func liveRDSExists(api clone.RDSRestoreClient) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, ref string) (bool, error) {
		in, err := api.DescribeInstance(ctx, ref)
		if errors.Is(err, clone.ErrRDSNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return in.Status != "deleting", nil
	}
}
