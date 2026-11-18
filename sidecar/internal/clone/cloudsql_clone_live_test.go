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

// lab-05e CHECK-37 (OWNER ACTION, decision D2): the clone contract on a
// real, funded Cloud SQL for PostgreSQL instance. Each check clones a
// whole billable instance holding production data, so it runs only with
// SAGE_CLOUDSQL_LIVE=1 and
//
//	SAGE_CLOUDSQL_LIVE_DATABASE_URL  the monitored instance's IP DSN
//	SAGE_CLOUDSQL_PROJECT            its project
//	SAGE_CLOUDSQL_INSTANCE           its instance name
//	SAGE_TEST_DATABASE_URL           a scratch database for the receipts
//
// with Google credentials (GOOGLE_APPLICATION_CREDENTIALS or ADC).
// Afterwards `gcloud sql instances list` must show no sage-* instance.
const sqlLiveFlag = "SAGE_CLOUDSQL_LIVE"

func TestCloudSQLLive_ContractOnARealInstance(t *testing.T) {
	contracttest.RequireLive(t, sqlLiveFlag)
	dsn := os.Getenv("SAGE_CLOUDSQL_LIVE_DATABASE_URL")
	project, instance := os.Getenv("SAGE_CLOUDSQL_PROJECT"), os.Getenv("SAGE_CLOUDSQL_INSTANCE")
	if dsn == "" || project == "" || instance == "" {
		t.Fatal(sqlLiveFlag + " is set but SAGE_CLOUDSQL_LIVE_DATABASE_URL, " +
			"SAGE_CLOUDSQL_PROJECT or SAGE_CLOUDSQL_INSTANCE is not")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prodPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid SAGE_CLOUDSQL_LIVE_DATABASE_URL")
	}
	t.Cleanup(prodPool.Close)
	identity, err := clone.PoolIdentity(prodPool)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pool := sqlReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		p, err := clone.NewCloudSQLCloneProvider(clone.CloudSQLCloneConfig{Project: project,
			Instance: instance, Deadline: 2 * time.Hour, TTL: 4 * time.Hour,
			Token: func(ctx context.Context) (clone.CloudSQLTokenSource, error) {
				return cloudtel.NewGCPTokenSource(ctx, cloudtel.GCPAuthOptions{
					CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")})
			},
			Production: func(context.Context) (clone.CloudSQLProduction, error) {
				return clone.CloudSQLProduction{DSN: dsn}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.Subject{Adapter: clone.CloudSQLCloneAdapter, Provider: p,
			Production: identity, Exists: func(ctx context.Context, ref string) (bool, error) {
				_, found, err := p.FindByName(ctx, ref)
				return found, err
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: sqlDeployment})
}
