package clone_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// lab-05f CHECK-39 (OWNER ACTION, decision D2): the clone contract on a
// real, funded AlloyDB cluster with continuous backup on. Each check
// restores a whole billable cluster holding production data, so it runs
// only with SAGE_ALLOYDB_LIVE=1 and
//
//	SAGE_ALLOYDB_LIVE_DATABASE_URL  the monitored primary instance's IP DSN
//	SAGE_ALLOYDB_PROJECT            its project
//	SAGE_ALLOYDB_LOCATION           its region
//	SAGE_ALLOYDB_CLUSTER            its cluster id
//	SAGE_TEST_DATABASE_URL          a scratch database for the receipts
//
// with Google credentials (GOOGLE_APPLICATION_CREDENTIALS or ADC).
// Afterwards no sage-* cluster may be listed by gcloud alloydb clusters list.
const adbLiveFlag = "SAGE_ALLOYDB_LIVE"

func TestAlloyDBLive_ContractOnARealCluster(t *testing.T) {
	contracttest.RequireLive(t, adbLiveFlag)
	dsn := os.Getenv("SAGE_ALLOYDB_LIVE_DATABASE_URL")
	project, location := os.Getenv("SAGE_ALLOYDB_PROJECT"), os.Getenv("SAGE_ALLOYDB_LOCATION")
	cluster := os.Getenv("SAGE_ALLOYDB_CLUSTER")
	if dsn == "" || project == "" || location == "" || cluster == "" {
		t.Fatal(adbLiveFlag + " is set but SAGE_ALLOYDB_LIVE_DATABASE_URL, " +
			"SAGE_ALLOYDB_PROJECT, SAGE_ALLOYDB_LOCATION or SAGE_ALLOYDB_CLUSTER is not")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prodPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid SAGE_ALLOYDB_LIVE_DATABASE_URL")
	}
	t.Cleanup(prodPool.Close)
	identity, err := clone.PoolIdentity(prodPool)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pool := adbReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		p, err := clone.NewAlloyDBRestoreProvider(clone.AlloyDBRestoreConfig{
			Project: project, Location: location, Cluster: cluster,
			Deadline: 2 * time.Hour, TTL: 4 * time.Hour,
			Token: func(ctx context.Context) (clone.CloudSQLTokenSource, error) {
				return cloudtel.NewGCPTokenSource(ctx, cloudtel.GCPAuthOptions{
					CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")})
			},
			Production: func(context.Context) (clone.AlloyDBProduction, error) {
				return clone.AlloyDBProduction{DSN: dsn}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.Subject{Adapter: clone.AlloyDBRestoreAdapter, Provider: p,
			Production: identity, Exists: func(ctx context.Context, ref string) (bool, error) {
				_, found, err := p.FindByName(ctx, ref)
				return found, err
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: adbDeployment})
}
